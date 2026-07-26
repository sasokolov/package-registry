package repl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/state"
)

// Peer is a configured replication partner.
type Peer struct {
	Name         string
	URL          string
	PullInterval time.Duration
}

// Client talks to one peer's internal API.
type Client struct {
	peer   Peer
	http   *http.Client
	authz  func(*http.Request)
	logger *slog.Logger

	// pinnedUUID is learned at the first handshake; a peer that changes its
	// UUID is a different site wearing the same name and is refused.
	pinnedUUID string
}

// NewClient builds a peer client.
func NewClient(peer Peer, httpClient *http.Client, authz func(*http.Request), logger *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	if authz == nil {
		authz = func(*http.Request) {}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{peer: peer, http: httpClient, authz: authz, logger: logger}
}

func (c *Client) do(ctx context.Context, path string, query url.Values) (*http.Response, error) {
	u := c.peer.URL + InternalPrefix + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request to peer %s: %w", c.peer.Name, err)
	}
	c.authz(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("peer %s unreachable: %w", c.peer.Name, err)
	}
	return resp, nil
}

// Status fetches the peer handshake, pinning its UUID on first contact.
func (c *Client) Status(ctx context.Context) (StatusResponse, error) {
	resp, err := c.do(ctx, "/status", nil)
	if err != nil {
		return StatusResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return StatusResponse{}, fmt.Errorf("peer %s status returned %d", c.peer.Name, resp.StatusCode)
	}
	var out StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return StatusResponse{}, fmt.Errorf("decode peer status: %w", err)
	}
	if out.Site != c.peer.Name {
		return StatusResponse{}, fmt.Errorf(
			"peer %s identifies as site %q: refusing to replicate a misconfigured peer", c.peer.Name, out.Site)
	}
	if c.pinnedUUID == "" {
		c.pinnedUUID = out.UUID
	} else if out.UUID != c.pinnedUUID {
		return StatusResponse{}, fmt.Errorf(
			"peer %s changed its site UUID (%s -> %s): a different site is using this name; run `registry repl trust-reset` if this is intentional",
			c.peer.Name, short(c.pinnedUUID), short(out.UUID))
	}
	return out, nil
}

// ErrResync means the cursor fell behind the peer's journal retention and a
// snapshot bootstrap is required.
var ErrResync = errors.New("cursor beyond retained journal, resync required")

// Journal fetches entries after a sequence.
func (c *Client) Journal(ctx context.Context, origin string, after int64, limit int) (JournalResponse, error) {
	q := url.Values{}
	q.Set("origin", origin)
	q.Set("after", strconv.FormatInt(after, 10))
	q.Set("limit", strconv.Itoa(limit))
	resp, err := c.do(ctx, "/journal", q)
	if err != nil {
		return JournalResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusGone {
		return JournalResponse{}, ErrResync
	}
	if resp.StatusCode != http.StatusOK {
		return JournalResponse{}, fmt.Errorf("peer %s journal returned %d", c.peer.Name, resp.StatusCode)
	}
	var out JournalResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return JournalResponse{}, fmt.Errorf("decode peer journal: %w", err)
	}
	// Origin spoofing check: a peer may only serve entries of the origin it
	// was asked for, and must never inject entries attributed elsewhere.
	for _, e := range out.Entries {
		if e.OriginSite != origin {
			return JournalResponse{}, fmt.Errorf(
				"peer %s served an entry attributed to origin %q while serving %q: refusing",
				c.peer.Name, e.OriginSite, origin)
		}
	}
	return out, nil
}

// Snapshot fetches the peer's full replicable state.
func (c *Client) Snapshot(ctx context.Context) (SnapshotResponse, error) {
	resp, err := c.do(ctx, "/snapshot", nil)
	if err != nil {
		return SnapshotResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return SnapshotResponse{}, fmt.Errorf("peer %s snapshot returned %d", c.peer.Name, resp.StatusCode)
	}
	var out SnapshotResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return SnapshotResponse{}, fmt.Errorf("decode peer snapshot: %w", err)
	}
	return out, nil
}

// FetchBlob streams a blob from the peer into the local store, verifying the
// digest while writing: the key IS the checksum, so a corrupted or hostile
// transfer cannot be stored (invariant 5).
func (c *Client) FetchBlob(ctx context.Context, store api.BlobStore, digest string, size int64) error {
	resp, err := c.do(ctx, "/blobs/sha256/"+digest, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("peer %s does not have blob %s: %w", c.peer.Name, short(digest), api.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer %s blob fetch returned %d", c.peer.Name, resp.StatusCode)
	}

	// Spool while hashing so nothing enters the store unverified.
	tmp, err := os.CreateTemp("", "registry-peer-blob-*")
	if err != nil {
		return fmt.Errorf("spool peer blob: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if err != nil {
		return fmt.Errorf("read peer blob: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != digest {
		return fmt.Errorf("peer %s served blob %s but the bytes hash to %s: %w",
			c.peer.Name, short(digest), short(got), api.ErrChecksumMismatch)
	}
	if size > 0 && written != size {
		return fmt.Errorf("peer %s served %d bytes for blob %s, expected %d",
			c.peer.Name, written, short(digest), size)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind peer blob: %w", err)
	}
	if err := store.Put(ctx, "blobs/sha256/"+digest, tmp,
		api.PutOpts{SHA256: digest, Size: written}); err != nil {
		return fmt.Errorf("store peer blob: %w", err)
	}
	return nil
}

// Manifest asks the peer for one hosted coordinate.
func (c *Client) Manifest(ctx context.Context, feed, path string) (ManifestResponse, error) {
	q := url.Values{}
	q.Set("feed", feed)
	q.Set("path", path)
	resp, err := c.do(ctx, "/manifest", q)
	if err != nil {
		return ManifestResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return ManifestResponse{}, api.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return ManifestResponse{}, fmt.Errorf("peer %s manifest lookup returned %d", c.peer.Name, resp.StatusCode)
	}
	var out ManifestResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ManifestResponse{}, fmt.Errorf("decode peer manifest: %w", err)
	}
	if len(out.SHA256) != 64 {
		return ManifestResponse{}, fmt.Errorf("peer %s returned an implausible digest", c.peer.Name)
	}
	return out, nil
}

// ForwardPublish sends a write to this peer because it is the feed's home
// site. Authentication is the replication credential; the publisher's
// identity travels as on-behalf-of headers.
func (c *Client) ForwardPublish(ctx context.Context, feed, path, method string,
	body io.Reader, identity, projectPath string) (int, []byte, error) {
	q := url.Values{}
	q.Set("feed", feed)
	q.Set("path", path)
	target := c.peer.URL + InternalPrefix + "/publish?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return 0, nil, fmt.Errorf("build forwarded publish: %w", err)
	}
	c.authz(req)
	req.Header.Set("X-Registry-On-Behalf-Of", identity)
	req.Header.Set("X-Registry-Forwarded-Method", method)
	if projectPath != "" {
		req.Header.Set("X-Registry-On-Behalf-Of-Project", projectPath)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("forward publish to %s: %w", c.peer.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

// Nudge tells the peer that we have new events (an optimization).
func (c *Client) Nudge(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.peer.URL+InternalPrefix+"/nudge", nil)
	if err != nil {
		return
	}
	c.authz(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// Name is the peer's configured name.
func (c *Client) Name() string { return c.peer.Name }

// Interval is the configured poll interval.
func (c *Client) Interval() time.Duration {
	if c.peer.PullInterval > 0 {
		return c.peer.PullInterval
	}
	return 10 * time.Second
}

var _ = state.JournalEntry{} // keeps the state import meaningful for readers
