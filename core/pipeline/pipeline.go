// Package pipeline is the generic read-path shared by every format:
//
//	cache lookup → singleflight → cross-replica lock → upstream fetch
//	(retry+jitter, circuit breaker, rate limit) → checksum verification →
//	store → serve
//
// Immutable artifacts are content-addressed: the blob lives at
// blobs/sha256/<hex> and the coordinate is a small manifest pointing at it
// (invariant 10). Mutable metadata is cached with the intent's TTL and
// served stale when the upstream is unavailable (invariant 6). Every result
// carries its source for the X-Registry-Source header (invariant 11).
package pipeline

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // legacy upstream checksums, not used for security
	"crypto/sha1" //nolint:gosec // legacy upstream checksums, not used for security
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sasokolov/package-registry/core/api"
)

// ingestTimeout bounds a detached artifact ingest (download of one blob).
const ingestTimeout = 15 * time.Minute

// LockFunc runs fn under a cross-replica lock. Implementations must degrade
// to running fn without the lock when the lock backend is down (invariant 7).
type LockFunc func(ctx context.Context, key string, fn func(ctx context.Context) error) error

// Options wires a Pipeline.
type Options struct {
	Store   api.BlobStore
	Lock    LockFunc // nil: in-process singleflight only
	Logger  *slog.Logger
	Metrics *Metrics         // nil: metrics disabled
	Now     func() time.Time // nil: time.Now
}

// Pipeline executes intents against cache and upstream.
type Pipeline struct {
	store   api.BlobStore
	lock    LockFunc
	logger  *slog.Logger
	metrics *Metrics
	now     func() time.Time
	sf      singleflight.Group
}

// New builds a Pipeline.
func New(o Options) *Pipeline {
	p := &Pipeline{
		store:   o.Store,
		lock:    o.Lock,
		logger:  o.Logger,
		metrics: o.Metrics,
		now:     o.Now,
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.lock == nil {
		p.lock = func(ctx context.Context, _ string, fn func(context.Context) error) error {
			return fn(ctx)
		}
	}
	return p
}

// Request is one intent to serve.
type Request struct {
	Feed     api.Feed
	Intent   api.Intent
	Module   api.FormatModule
	Upstream *Upstream // nil for hosted-only feeds
}

// Result is a streamable response with its provenance.
type Result struct {
	Body    io.ReadCloser
	Size    int64 // -1 when unknown
	SHA256  string
	ModTime time.Time
	Source  api.Source
}

// manifest is the pointer from a coordinate to its content-addressed blob.
type manifest struct {
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	IngestedAt time.Time `json:"ingested_at"`
}

// Serve executes the intent.
func (p *Pipeline) Serve(ctx context.Context, req Request) (*Result, error) {
	if err := validRemotePath(req.Intent.RemotePath); err != nil {
		return nil, err
	}
	var res *Result
	var err error
	switch req.Intent.Kind {
	case api.IntentArtifact:
		res, err = p.serveArtifact(ctx, req)
	case api.IntentMetadata:
		res, err = p.serveMetadata(ctx, req)
	default:
		return nil, fmt.Errorf("unknown intent kind %q", req.Intent.Kind)
	}
	if err != nil {
		p.metrics.failure(req.Feed.Name, failureReason(err))
		return nil, err
	}
	p.metrics.request(req.Feed.Name, string(res.Source))
	return res, nil
}

func failureReason(err error) string {
	switch {
	case errors.Is(err, api.ErrNotFound):
		return "not_found"
	case errors.Is(err, api.ErrChecksumMismatch):
		return "checksum_mismatch"
	case errors.Is(err, api.ErrUpstreamUnavailable):
		return "upstream_unavailable"
	default:
		return "other"
	}
}

func validRemotePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") {
		return api.NotFoundf("invalid path %q", p)
	}
	if clean := path.Clean(p); clean != p || clean == "." || strings.HasPrefix(clean, "../") {
		return api.NotFoundf("invalid path %q", p)
	}
	return nil
}

func manifestKey(feed api.Feed, intent api.Intent) string {
	return "manifests/" + feed.Name + "/" + intent.RemotePath
}

func metaKey(feed api.Feed, intent api.Intent) string {
	return "meta/" + feed.Name + "/" + intent.RemotePath
}

func blobKey(sha string) string { return "blobs/sha256/" + sha }

// ---------------------------------------------------------------------------
// Immutable artifacts

func (p *Pipeline) serveArtifact(ctx context.Context, req Request) (*Result, error) {
	mkey := manifestKey(req.Feed, req.Intent)

	if m, err := p.loadManifest(ctx, mkey); err == nil {
		return p.openBlob(ctx, m, api.SourceCache)
	} else if !errors.Is(err, api.ErrNotFound) {
		return nil, err
	}

	if req.Upstream == nil {
		return nil, api.NotFoundf("artifact %s not hosted here", req.Intent.Coord)
	}

	// One ingest per key per process; the winner's result is shared.
	v, err, _ := p.sf.Do("artifact\x00"+mkey, func() (any, error) {
		// Detach from the first caller so its disconnect does not kill the
		// download other waiters share; bound it instead.
		ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ingestTimeout)
		defer cancel()
		return p.ingestArtifact(ictx, req, mkey)
	})
	if err != nil {
		return nil, err
	}
	return p.openBlob(ctx, v.(manifest), api.SourceUpstream)
}

func (p *Pipeline) loadManifest(ctx context.Context, key string) (manifest, error) {
	rc, _, err := p.store.Get(ctx, key)
	if err != nil {
		return manifest{}, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := readAllCapped(rc, 1<<20)
	if err != nil {
		return manifest{}, fmt.Errorf("read manifest %s: %w", key, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return manifest{}, fmt.Errorf("decode manifest %s: %w", key, err)
	}
	if m.SHA256 == "" {
		return manifest{}, fmt.Errorf("manifest %s has no digest", key)
	}
	return m, nil
}

func (p *Pipeline) openBlob(ctx context.Context, m manifest, source api.Source) (*Result, error) {
	rc, info, err := p.store.Get(ctx, blobKey(m.SHA256))
	if err != nil {
		return nil, fmt.Errorf("manifest points to missing blob %s: %w", m.SHA256, err)
	}
	return &Result{
		Body:    rc,
		Size:    m.Size,
		SHA256:  m.SHA256,
		ModTime: info.ModTime,
		Source:  source,
	}, nil
}

// ingestArtifact downloads, verifies and stores one artifact, returning its
// manifest. Runs under the cross-replica lock and re-checks the manifest
// after acquiring it (another replica may have won the race). Degradation
// when the lock backend is down is the LockFunc implementation's job.
func (p *Pipeline) ingestArtifact(ctx context.Context, req Request, mkey string) (manifest, error) {
	var out manifest
	do := func(ctx context.Context) error {
		if m, err := p.loadManifest(ctx, mkey); err == nil {
			out = m // another replica ingested while we waited for the lock
			return nil
		}
		m, err := p.fetchAndStore(ctx, req, mkey)
		if err != nil {
			return err
		}
		out = m
		return nil
	}
	if err := p.lock(ctx, mkey, do); err != nil {
		return manifest{}, err
	}
	return out, nil
}

func (p *Pipeline) fetchAndStore(ctx context.Context, req Request, mkey string) (manifest, error) {
	resp, err := req.Upstream.Fetch(ctx, req.Intent.RemotePath)
	if err != nil {
		return manifest{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	// Spool to a temp file while hashing so the checksum is verified before
	// anything becomes visible in the store (invariant 5).
	tmp, err := os.CreateTemp("", "registry-ingest-*")
	if err != nil {
		return manifest{}, fmt.Errorf("ingest spool: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	sha := sha256.New()
	writers := []io.Writer{tmp, sha}
	extra := extraHasher(req.Intent.Checksum.Algo)
	if extra != nil {
		writers = append(writers, extra)
	}
	size, err := io.Copy(io.MultiWriter(writers...), resp.Body)
	if err != nil {
		return manifest{}, fmt.Errorf("ingest %s: download: %w", req.Intent.Coord, err)
	}

	digest := hex.EncodeToString(sha.Sum(nil))
	if err := verifyChecksum(req.Intent.Checksum, digest, extra); err != nil {
		p.logger.Error("checksum mismatch, artifact rejected",
			"feed", req.Feed.Name, "coord", req.Intent.Coord.String(), "error", err)
		return manifest{}, err
	}

	bkey := blobKey(digest)
	if _, err := p.store.Stat(ctx, bkey); errors.Is(err, api.ErrNotFound) {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return manifest{}, fmt.Errorf("ingest rewind: %w", err)
		}
		if err := p.store.Put(ctx, bkey, tmp, api.PutOpts{SHA256: digest, Size: size}); err != nil {
			return manifest{}, fmt.Errorf("store blob: %w", err)
		}
	} else if err != nil {
		return manifest{}, fmt.Errorf("stat blob: %w", err)
	}

	m := manifest{SHA256: digest, Size: size, IngestedAt: p.now().UTC()}
	raw, err := json.Marshal(m)
	if err != nil {
		return manifest{}, fmt.Errorf("encode manifest: %w", err)
	}
	if err := p.store.Put(ctx, mkey, bytes.NewReader(raw), api.PutOpts{}); err != nil {
		return manifest{}, fmt.Errorf("store manifest: %w", err)
	}
	p.logger.Info("artifact ingested",
		"feed", req.Feed.Name, "coord", req.Intent.Coord.String(), "sha256", digest, "size", size)
	return m, nil
}

func extraHasher(algo string) hash.Hash {
	switch algo {
	case "sha1":
		return sha1.New() //nolint:gosec // upstream-provided legacy checksum
	case "md5":
		return md5.New() //nolint:gosec // upstream-provided legacy checksum
	default:
		return nil
	}
}

func verifyChecksum(want api.Checksum, sha256hex string, extra hash.Hash) error {
	if want.IsZero() {
		return nil
	}
	got := sha256hex
	if want.Algo != "sha256" {
		if extra == nil {
			return fmt.Errorf("unsupported checksum algo %q: %w", want.Algo, api.ErrChecksumMismatch)
		}
		got = hex.EncodeToString(extra.Sum(nil))
	}
	if !strings.EqualFold(got, want.Hex) {
		return fmt.Errorf("%s digest %s does not match expected %s: %w",
			want.Algo, got, want.Hex, api.ErrChecksumMismatch)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Mutable metadata (TTL + stale-while-revalidate)

func (p *Pipeline) serveMetadata(ctx context.Context, req Request) (*Result, error) {
	key := metaKey(req.Feed, req.Intent)

	info, statErr := p.store.Stat(ctx, key)
	cached := statErr == nil
	fresh := cached && req.Intent.CacheTTL > 0 && p.now().Sub(info.ModTime) < req.Intent.CacheTTL
	if fresh {
		return p.openMeta(ctx, key, api.SourceCache)
	}
	if req.Upstream == nil {
		if cached {
			return p.openMeta(ctx, key, api.SourceLocal)
		}
		return nil, api.NotFoundf("metadata %s not hosted here", req.Intent.Coord)
	}

	_, err, _ := p.sf.Do("meta\x00"+key, func() (any, error) {
		return nil, p.refreshMetadata(ctx, req, key)
	})
	switch {
	case err == nil:
		return p.openMeta(ctx, key, api.SourceUpstream)
	case errors.Is(err, api.ErrNotFound):
		// The upstream answered: it is gone. Do not mask deletions with
		// stale copies.
		return nil, err
	case cached:
		// Upstream unavailable: serve stale (invariant 6).
		p.logger.Warn("upstream unavailable, serving stale metadata",
			"feed", req.Feed.Name, "path", req.Intent.RemotePath, "error", err)
		return p.openMeta(ctx, key, api.SourceStale)
	default:
		return nil, err
	}
}

func (p *Pipeline) refreshMetadata(ctx context.Context, req Request, key string) error {
	body, err := req.Upstream.FetchAll(ctx, req.Intent.RemotePath)
	if err != nil {
		return err
	}
	rewritten, err := req.Module.RewriteMetadata(req.Feed, body)
	if err != nil {
		return fmt.Errorf("rewrite metadata %s: %w", req.Intent.Coord, err)
	}
	if err := p.store.Put(ctx, key, bytes.NewReader(rewritten), api.PutOpts{}); err != nil {
		return fmt.Errorf("store metadata: %w", err)
	}
	return nil
}

func (p *Pipeline) openMeta(ctx context.Context, key string, source api.Source) (*Result, error) {
	rc, info, err := p.store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return &Result{
		Body:    rc,
		Size:    info.Size,
		SHA256:  info.SHA256,
		ModTime: info.ModTime,
		Source:  source,
	}, nil
}
