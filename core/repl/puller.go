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

// controlTimeout bounds status, journal and manifest calls. It is short on
// purpose: an unreachable peer must fail fast, because the poll loop holds
// a cross-replica lease while it runs and a hung handshake would stall the
// whole site's replication with it. Blob transfers keep the client's long
// timeout.
const controlTimeout = 10 * time.Second

// snapshotTimeout bounds a bootstrap: the whole hosted manifest set of a
// large site travels as one document, which the control timeout would cut
// short on every attempt.
const snapshotTimeout = 10 * time.Minute

// blobTimeout bounds a single blob transfer attempt. Resume makes the retry
// cheap, so this only has to be long enough for steady progress.
const blobTimeout = 30 * time.Minute

// Response bounds. A peer is authenticated but not trusted with this
// site's memory: every JSON body is read through a limit.
const (
	maxControlResponse  = 32 << 20  // status, journal page, manifest lookup
	maxSnapshotResponse = 512 << 20 // a whole site's manifest set
)

// Client talks to one peer's internal API.
type Client struct {
	peer   Peer
	http   *http.Client
	authz  func(*http.Request)
	logger *slog.Logger
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
	return c.doWithTimeout(ctx, path, query, controlTimeout)
}

func (c *Client) doWithTimeout(ctx context.Context, path string, query url.Values,
	timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	u := c.peer.URL + InternalPrefix + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("build request to peer %s: %w", c.peer.Name, err)
	}
	c.authz(req)
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("peer %s unreachable: %w", c.peer.Name, err)
	}
	// The body outlives this call; cancel when the caller closes it.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnClose ties a request context's cancellation to the body's Close,
// so the timeout covers reading the response as well.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
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
	if err := decodeJSON(resp.Body, maxControlResponse, &out); err != nil {
		return StatusResponse{}, fmt.Errorf("decode peer status: %w", err)
	}
	if out.Site != c.peer.Name {
		return StatusResponse{}, fmt.Errorf(
			"peer %s identifies as site %q: refusing to replicate a misconfigured peer", c.peer.Name, out.Site)
	}
	if len(out.UUID) == 0 {
		return StatusResponse{}, fmt.Errorf("peer %s reported no site UUID", c.peer.Name)
	}
	// The pin lives in the database, not in this process: see
	// DB.PinPeerIdentity. The manager applies it right after the handshake.
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
	if err := decodeJSON(resp.Body, maxControlResponse, &out); err != nil {
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
	resp, err := c.doWithTimeout(ctx, "/snapshot", nil, snapshotTimeout)
	if err != nil {
		return SnapshotResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return SnapshotResponse{}, fmt.Errorf("peer %s snapshot returned %d", c.peer.Name, resp.StatusCode)
	}
	var out SnapshotResponse
	if err := decodeJSON(resp.Body, maxSnapshotResponse, &out); err != nil {
		return SnapshotResponse{}, fmt.Errorf("decode peer snapshot: %w", err)
	}
	return out, nil
}

// FetchBlob streams a blob from the peer into the local store, verifying the
// digest while writing: the key IS the checksum, so a corrupted or hostile
// transfer cannot be stored (invariant 5). An interrupted transfer is
// resumed with a Range request rather than restarted, which matters for
// large artifacts over a WAN.
func (c *Client) FetchBlob(ctx context.Context, store api.BlobStore, digest string, size int64) error {
	// Spool while hashing so nothing enters the store unverified.
	tmp, err := os.CreateTemp("", "registry-peer-blob-*")
	if err != nil {
		return fmt.Errorf("spool peer blob: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	// A hostile or broken peer must not be able to spool unbounded data
	// before the digest is checked. The expected size is known up front;
	// allow a small slack for a peer that reports it imprecisely.
	limit := size + 1<<20
	if size <= 0 {
		limit = maxUnsizedBlob
	}

	h := sha256.New()
	var written int64
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if written > limit {
			return fmt.Errorf("peer %s sent more than the declared %d bytes for blob %s",
				c.peer.Name, size, short(digest))
		}
		n, err := c.streamBlob(ctx, digest, written, io.MultiWriter(tmp, h), limit-written)
		written += n
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		if errors.Is(err, api.ErrNotFound) || n == 0 {
			// Nothing was transferred: retrying the same range is futile.
			break
		}
		c.logger.Debug("resuming interrupted peer blob transfer",
			"peer", c.peer.Name, "digest", short(digest), "offset", written, "error", err)
	}
	if lastErr != nil {
		return lastErr
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

// maxUnsizedBlob bounds a transfer whose size the peer did not declare.
const maxUnsizedBlob = 8 << 30

// streamBlob copies one (possibly partial) transfer into dst, starting at
// offset and accepting at most limit bytes. It returns how many bytes it
// wrote, so the caller can resume.
func (c *Client) streamBlob(ctx context.Context, digest string, offset int64, dst io.Writer, limit int64) (int64, error) {
	// A blob transfer gets its own deadline: long enough for a large
	// artifact over a WAN, but never unbounded.
	ctx, cancel := context.WithTimeout(ctx, blobTimeout)
	defer cancel()

	target := c.peer.URL + InternalPrefix + "/blobs/sha256/" + digest
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, fmt.Errorf("build blob request: %w", err)
	}
	c.authz(req)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("peer %s blob transfer: %w", c.peer.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return 0, fmt.Errorf("peer %s does not have blob %s: %w",
			c.peer.Name, short(digest), api.ErrNotFound)
	case offset > 0 && resp.StatusCode == http.StatusOK:
		// The peer ignored the range; restarting from zero would corrupt
		// the hash we already fed, so fail and let the caller start over.
		return 0, fmt.Errorf("peer %s does not support resume for blob %s",
			c.peer.Name, short(digest))
	case offset > 0 && resp.StatusCode != http.StatusPartialContent:
		return 0, fmt.Errorf("peer %s resume returned %d", c.peer.Name, resp.StatusCode)
	case offset == 0 && resp.StatusCode != http.StatusOK:
		return 0, fmt.Errorf("peer %s blob fetch returned %d", c.peer.Name, resp.StatusCode)
	}

	n, err := io.Copy(dst, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return n, fmt.Errorf("read peer blob: %w", err)
	}
	if n > limit {
		return n, fmt.Errorf("peer %s exceeded the declared size for blob %s",
			c.peer.Name, short(digest))
	}
	return n, nil
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
	if err := decodeJSON(resp.Body, maxControlResponse, &out); err != nil {
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
	body io.Reader, header http.Header, identity, projectPath string) (int, http.Header, []byte, error) {
	q := url.Values{}
	q.Set("feed", feed)
	q.Set("path", path)
	target := c.peer.URL + InternalPrefix + "/publish?" + q.Encode()

	// A forwarded publish carries an artifact body: it needs the transfer
	// budget, not the control-call one.
	ctx, cancel := context.WithTimeout(ctx, blobTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("build forwarded publish: %w", err)
	}
	c.authz(req)
	// The payload's own headers ride along: a body whose media type was
	// dropped on the way is a body the home site cannot read (the caller
	// filters them; nothing about the client's identity is in here).
	for name, values := range header {
		req.Header[name] = values
	}
	req.Header.Set("X-Registry-On-Behalf-Of", identity)
	req.Header.Set("X-Registry-Forwarded-Method", method)
	if projectPath != "" {
		req.Header.Set("X-Registry-On-Behalf-Of-Project", projectPath)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("forward publish to %s: %w", c.peer.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, out, nil
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

// decodeJSON reads at most limit bytes and rejects anything longer, so an
// oversized (or endless) response fails instead of exhausting memory.
func decodeJSON(body io.Reader, limit int64, out any) error {
	limited := io.LimitReader(body, limit+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if int64(len(raw)) > limit {
		return fmt.Errorf("response exceeds the %d byte limit", limit)
	}
	return json.Unmarshal(raw, out)
}

// SameEndpoint reports whether two clients address the same peer the same
// way; an unchanged peer keeps its pinned site UUID across config reloads.
func (c *Client) SameEndpoint(other *Client) bool {
	return c.peer.Name == other.peer.Name &&
		c.peer.URL == other.peer.URL &&
		c.peer.PullInterval == other.peer.PullInterval
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
