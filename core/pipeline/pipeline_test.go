package pipeline

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // legacy checksum in tests
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

// ---------------------------------------------------------------------------
// fake in-memory BlobStore

type memBlob struct {
	data    []byte
	sha256  string
	modTime time.Time
}

type memStore struct {
	mu    sync.Mutex
	blobs map[string]memBlob
	now   func() time.Time
}

func newMemStore(now func() time.Time) *memStore {
	if now == nil {
		now = time.Now
	}
	return &memStore{blobs: map[string]memBlob{}, now: now}
}

func (s *memStore) Get(_ context.Context, key string) (io.ReadCloser, api.BlobInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[key]
	if !ok {
		return nil, api.BlobInfo{}, api.NotFoundf("blob %s", key)
	}
	return io.NopCloser(bytes.NewReader(b.data)), s.info(key, b), nil
}

func (s *memStore) info(key string, b memBlob) api.BlobInfo {
	return api.BlobInfo{Key: key, Size: int64(len(b.data)), SHA256: b.sha256, ModTime: b.modTime}
}

func (s *memStore) Put(_ context.Context, key string, r io.Reader, opts api.PutOpts) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if opts.SHA256 != "" && !strings.EqualFold(opts.SHA256, digest) {
		return api.ErrChecksumMismatch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[key] = memBlob{data: data, sha256: digest, modTime: s.now()}
	return nil
}

func (s *memStore) Stat(_ context.Context, key string) (api.BlobInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[key]
	if !ok {
		return api.BlobInfo{}, api.NotFoundf("blob %s", key)
	}
	return s.info(key, b), nil
}

func (s *memStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.blobs[key]; !ok {
		return api.NotFoundf("blob %s", key)
	}
	delete(s.blobs, key)
	return nil
}

func (s *memStore) List(context.Context, string) (api.Iter[api.BlobInfo], error) {
	return nil, errors.New("not implemented in fake")
}

func (s *memStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.blobs[key]
	return ok
}

// ---------------------------------------------------------------------------
// fake format module

type echoModule struct{ rewriteSuffix string }

func (m echoModule) Name() string { return "echo" }
func (m echoModule) Routes() []api.Route {
	return []api.Route{{Method: http.MethodGet, Pattern: "/*"}}
}
func (m echoModule) Parse(*http.Request) (api.Intent, error) { return api.Intent{}, nil }
func (m echoModule) RewriteMetadata(_ api.Feed, body []byte) ([]byte, error) {
	if m.rewriteSuffix == "" {
		return body, nil
	}
	return append(append([]byte{}, body...), []byte(m.rewriteSuffix)...), nil
}

// ---------------------------------------------------------------------------
// harness

type harness struct {
	t        *testing.T
	store    *memStore
	pipe     *Pipeline
	server   *httptest.Server
	requests atomic.Int64
	fail     atomic.Bool // upstream returns 500 when set
	delay    time.Duration
	content  map[string]string
	custom   map[string]http.HandlerFunc
	mu       sync.Mutex
	now      time.Time
	nowMu    sync.Mutex
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, content: map[string]string{}, custom: map[string]http.HandlerFunc{}, now: time.Unix(1_700_000_000, 0)}
	h.store = newMemStore(h.clock)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		if h.delay > 0 {
			time.Sleep(h.delay)
		}
		if h.fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		h.mu.Lock()
		custom := h.custom[p]
		body, ok := h.content[p]
		h.mu.Unlock()
		if custom != nil {
			custom(w, r)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)

	h.pipe = New(Options{
		Store:  h.store,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Now:    h.clock,
	})
	return h
}

func (h *harness) clock() time.Time {
	h.nowMu.Lock()
	defer h.nowMu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.nowMu.Lock()
	h.now = h.now.Add(d)
	h.nowMu.Unlock()
}

func (h *harness) set(path, body string) {
	h.mu.Lock()
	h.content[path] = body
	h.mu.Unlock()
}

func (h *harness) upstream(rps float64) *Upstream {
	u, err := NewUpstream(UpstreamOptions{
		Feed:    "test-feed",
		BaseURL: h.server.URL,
		RPS:     rps,
		Client:  h.server.Client(),
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Now:     h.clock,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return u
}

func (h *harness) request(intent api.Intent, up *Upstream) Request {
	return Request{
		Feed:     api.Feed{Name: "test-feed", Format: "echo"},
		Intent:   intent,
		Module:   echoModule{},
		Upstream: up,
	}
}

func artifactIntent(path string) api.Intent {
	return api.Intent{
		Kind:       api.IntentArtifact,
		Coord:      api.PackageCoordinate{Format: "echo", Name: path},
		RemotePath: path,
	}
}

func metadataIntent(path string) api.Intent {
	const ttl = time.Minute
	return api.Intent{
		Kind:       api.IntentMetadata,
		Coord:      api.PackageCoordinate{Format: "echo", Name: path},
		CacheTTL:   ttl,
		RemotePath: path,
	}
}

func mustBody(t *testing.T, res *Result) string {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// artifact scenarios

func TestArtifactMissThenHit(t *testing.T) {
	h := newHarness(t)
	h.set("libs/foo-1.0.jar", "jar bytes")
	up := h.upstream(0)
	ctx := t.Context()

	res, err := h.pipe.Serve(ctx, h.request(artifactIntent("libs/foo-1.0.jar"), up))
	if err != nil {
		t.Fatalf("miss serve: %v", err)
	}
	if res.Source != api.SourceUpstream {
		t.Errorf("first source = %s, want upstream", res.Source)
	}
	if got := mustBody(t, res); got != "jar bytes" {
		t.Errorf("body = %q", got)
	}
	if h.requests.Load() != 1 {
		t.Errorf("upstream requests = %d, want 1", h.requests.Load())
	}

	sum := sha256.Sum256([]byte("jar bytes"))
	if !h.store.has("blobs/sha256/" + hex.EncodeToString(sum[:])) {
		t.Error("content-addressed blob missing")
	}
	if !h.store.has("manifests/test-feed/libs/foo-1.0.jar") {
		t.Error("manifest missing")
	}

	// Hit: no new upstream traffic, source=cache.
	res, err = h.pipe.Serve(ctx, h.request(artifactIntent("libs/foo-1.0.jar"), up))
	if err != nil {
		t.Fatalf("hit serve: %v", err)
	}
	if res.Source != api.SourceCache {
		t.Errorf("second source = %s, want cache", res.Source)
	}
	if got := mustBody(t, res); got != "jar bytes" {
		t.Errorf("hit body = %q", got)
	}
	if h.requests.Load() != 1 {
		t.Errorf("upstream requests after hit = %d, want still 1", h.requests.Load())
	}
}

func TestArtifactConcurrentMissSingleUpstreamFetch(t *testing.T) {
	h := newHarness(t)
	h.set("big.bin", strings.Repeat("x", 1024))
	h.delay = 50 * time.Millisecond // widen the race window
	up := h.upstream(0)
	ctx := t.Context()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := h.pipe.Serve(ctx, h.request(artifactIntent("big.bin"), up))
			if err != nil {
				errs <- err
				return
			}
			if body := mustBody(t, res); len(body) != 1024 {
				errs <- errors.New("short body")
				return
			}
			errs <- nil
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := h.requests.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want exactly 1", got)
	}
}

func TestArtifactUpstreamDownFreshCache(t *testing.T) {
	h := newHarness(t)
	h.set("cached.jar", "payload")
	up := h.upstream(0)
	ctx := t.Context()

	if _, err := h.pipe.Serve(ctx, h.request(artifactIntent("cached.jar"), up)); err != nil {
		t.Fatal(err)
	}
	h.server.Close() // upstream is gone

	res, err := h.pipe.Serve(ctx, h.request(artifactIntent("cached.jar"), up))
	if err != nil {
		t.Fatalf("serve with dead upstream: %v", err)
	}
	if res.Source != api.SourceCache {
		t.Errorf("source = %s, want cache", res.Source)
	}
	if got := mustBody(t, res); got != "payload" {
		t.Errorf("body = %q", got)
	}
}

func TestArtifactChecksumMismatchNotStored(t *testing.T) {
	h := newHarness(t)
	h.set("evil.jar", "tampered content")
	up := h.upstream(0)
	ctx := t.Context()

	intent := artifactIntent("evil.jar")
	intent.Checksum = api.Checksum{Algo: "sha256", Hex: strings.Repeat("a", 64)}

	_, err := h.pipe.Serve(ctx, h.request(intent, up))
	if !errors.Is(err, api.ErrChecksumMismatch) {
		t.Fatalf("Serve = %v, want ErrChecksumMismatch", err)
	}
	if h.store.has("manifests/test-feed/evil.jar") {
		t.Error("manifest stored despite checksum mismatch")
	}
	sum := sha256.Sum256([]byte("tampered content"))
	if h.store.has("blobs/sha256/" + hex.EncodeToString(sum[:])) {
		t.Error("blob stored despite checksum mismatch")
	}
}

func TestArtifactSha1ChecksumVerified(t *testing.T) {
	h := newHarness(t)
	h.set("ok.jar", "correct content")
	up := h.upstream(0)
	ctx := t.Context()

	sum := sha1.Sum([]byte("correct content")) //nolint:gosec // matching legacy upstream checksum
	intent := artifactIntent("ok.jar")
	intent.Checksum = api.Checksum{Algo: "sha1", Hex: hex.EncodeToString(sum[:])}

	res, err := h.pipe.Serve(ctx, h.request(intent, up))
	if err != nil {
		t.Fatalf("Serve with correct sha1: %v", err)
	}
	if got := mustBody(t, res); got != "correct content" {
		t.Errorf("body = %q", got)
	}

	// Wrong sha1 must be rejected.
	h.set("bad.jar", "correct content")
	bad := artifactIntent("bad.jar")
	bad.Checksum = api.Checksum{Algo: "sha1", Hex: strings.Repeat("0", 40)}
	if _, err := h.pipe.Serve(ctx, h.request(bad, up)); !errors.Is(err, api.ErrChecksumMismatch) {
		t.Fatalf("Serve = %v, want ErrChecksumMismatch", err)
	}
}

func TestArtifact404NotCached(t *testing.T) {
	h := newHarness(t)
	up := h.upstream(0)
	ctx := t.Context()

	_, err := h.pipe.Serve(ctx, h.request(artifactIntent("nope.jar"), up))
	if !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("Serve = %v, want ErrNotFound", err)
	}

	// Once the file appears upstream it must be fetchable (no negative cache).
	h.set("nope.jar", "now exists")
	res, err := h.pipe.Serve(ctx, h.request(artifactIntent("nope.jar"), up))
	if err != nil {
		t.Fatalf("Serve after upstream gained the file: %v", err)
	}
	if got := mustBody(t, res); got != "now exists" {
		t.Errorf("body = %q", got)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	h := newHarness(t)
	up := h.upstream(0)
	for _, p := range []string{"../secret", "a/../../b", "/abs", ""} {
		intent := artifactIntent(p)
		if _, err := h.pipe.Serve(t.Context(), h.request(intent, up)); !errors.Is(err, api.ErrNotFound) {
			t.Errorf("path %q: err = %v, want ErrNotFound", p, err)
		}
	}
}

// ---------------------------------------------------------------------------
// metadata scenarios

func TestMetadataFreshCacheSkipsUpstream(t *testing.T) {
	h := newHarness(t)
	h.set("meta/index.json", `{"v":1}`)
	up := h.upstream(0)
	ctx := t.Context()

	res, err := h.pipe.Serve(ctx, h.request(metadataIntent("meta/index.json"), up))
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != api.SourceUpstream {
		t.Errorf("first source = %s", res.Source)
	}
	_ = mustBody(t, res)

	h.advance(10 * time.Second) // still fresh
	h.fail.Store(true)          // upstream would fail if contacted

	res, err = h.pipe.Serve(ctx, h.request(metadataIntent("meta/index.json"), up))
	if err != nil {
		t.Fatalf("fresh cache serve: %v", err)
	}
	if res.Source != api.SourceCache {
		t.Errorf("source = %s, want cache", res.Source)
	}
	if h.requests.Load() != 1 {
		t.Errorf("upstream requests = %d, want 1 (fresh cache must not revalidate)", h.requests.Load())
	}
}

func TestMetadataStaleWhenUpstreamDown(t *testing.T) {
	h := newHarness(t)
	h.set("meta/list.json", `{"versions":["1.0"]}`)
	up := h.upstream(0)
	ctx := t.Context()

	if _, err := h.pipe.Serve(ctx, h.request(metadataIntent("meta/list.json"), up)); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Minute) // cache is now stale
	h.fail.Store(true)         // upstream down (500s)

	res, err := h.pipe.Serve(ctx, h.request(metadataIntent("meta/list.json"), up))
	if err != nil {
		t.Fatalf("stale serve: %v", err)
	}
	if res.Source != api.SourceStale {
		t.Errorf("source = %s, want stale", res.Source)
	}
	if got := mustBody(t, res); got != `{"versions":["1.0"]}` {
		t.Errorf("stale body = %q", got)
	}
}

func TestMetadataStaleRefreshedWhenUpstreamRecovers(t *testing.T) {
	h := newHarness(t)
	h.set("meta/pkg.json", "v1")
	up := h.upstream(0)
	ctx := t.Context()

	if _, err := h.pipe.Serve(ctx, h.request(metadataIntent("meta/pkg.json"), up)); err != nil {
		t.Fatal(err)
	}
	h.set("meta/pkg.json", "v2")
	h.advance(2 * time.Minute)

	res, err := h.pipe.Serve(ctx, h.request(metadataIntent("meta/pkg.json"), up))
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != api.SourceUpstream {
		t.Errorf("source = %s, want upstream", res.Source)
	}
	if got := mustBody(t, res); got != "v2" {
		t.Errorf("body = %q, want refreshed v2", got)
	}
}

func TestMetadataRewriteApplied(t *testing.T) {
	h := newHarness(t)
	h.set("meta/root.json", "original")
	up := h.upstream(0)

	req := h.request(metadataIntent("meta/root.json"), up)
	req.Module = echoModule{rewriteSuffix: "+rewritten"}

	res, err := h.pipe.Serve(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustBody(t, res); got != "original+rewritten" {
		t.Errorf("body = %q, RewriteMetadata not applied before caching", got)
	}
}

func TestMetadata404NotMaskedByStale(t *testing.T) {
	h := newHarness(t)
	h.set("meta/gone.json", "soon gone")
	up := h.upstream(0)
	ctx := t.Context()

	if _, err := h.pipe.Serve(ctx, h.request(metadataIntent("meta/gone.json"), up)); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	delete(h.content, "meta/gone.json") // upstream now 404s
	h.mu.Unlock()
	h.advance(2 * time.Minute)

	if _, err := h.pipe.Serve(ctx, h.request(metadataIntent("meta/gone.json"), up)); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("Serve = %v, want ErrNotFound (deletions must not be stale-masked)", err)
	}
}

// ---------------------------------------------------------------------------
// upstream client behavior

func TestUpstreamRetriesTransientErrors(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			http.Error(w, "flaky", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "finally")
	}))
	defer srv.Close()

	u, err := NewUpstream(UpstreamOptions{
		Feed: "flaky", BaseURL: srv.URL, Client: srv.Client(),
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := u.Fetch(t.Context(), "thing", FetchOpts{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if calls.Load() != 3 {
		t.Errorf("attempts = %d, want 3 (two retries)", calls.Load())
	}
}

func TestCircuitBreakerOpensAndRecovers(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	b := NewBreaker(3, 10*time.Second, clock)

	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("closed breaker refused request %d", i)
		}
		b.Failure()
	}
	if b.State() != BreakerOpen {
		t.Fatalf("state after threshold failures = %d, want open", b.State())
	}
	if b.Allow() {
		t.Fatal("open breaker allowed a request before cooldown")
	}

	now = now.Add(11 * time.Second)
	if !b.Allow() {
		t.Fatal("breaker did not admit the half-open probe after cooldown")
	}
	if b.Allow() {
		t.Fatal("breaker admitted a second concurrent half-open probe")
	}
	b.Success()
	if b.State() != BreakerClosed || !b.Allow() {
		t.Fatal("successful probe did not close the breaker")
	}

	// A failing probe must reopen it.
	for i := 0; i < 3; i++ {
		b.Failure()
	}
	now = now.Add(11 * time.Second)
	if !b.Allow() {
		t.Fatal("no probe admitted")
	}
	b.Failure()
	if b.State() != BreakerOpen {
		t.Fatal("failed probe did not reopen the breaker")
	}
}

func TestBreakerShieldsUpstream(t *testing.T) {
	h := newHarness(t)
	h.fail.Store(true)
	up := h.upstream(0)
	ctx := t.Context()

	// Each Serve makes up to 3 attempts; two Serves exceed the threshold (5).
	for i := 0; i < 2; i++ {
		if _, err := h.pipe.Serve(ctx, h.request(artifactIntent("x.jar"), up)); err == nil {
			t.Fatal("expected failure")
		}
	}
	before := h.requests.Load()
	if _, err := h.pipe.Serve(ctx, h.request(artifactIntent("x.jar"), up)); !errors.Is(err, api.ErrUpstreamUnavailable) {
		t.Fatalf("Serve = %v, want ErrUpstreamUnavailable", err)
	}
	if h.requests.Load() != before {
		t.Errorf("open breaker still let %d request(s) through", h.requests.Load()-before)
	}
	if up.BreakerState() != BreakerOpen {
		t.Errorf("breaker state = %d, want open", up.BreakerState())
	}
}

func TestUpstreamRateLimit(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		h.set("f"+string(rune('0'+i)), "x")
	}
	up := h.upstream(100) // 100 rps, burst 1 → ~10ms between requests
	ctx := t.Context()

	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := h.pipe.Serve(ctx, h.request(artifactIntent("f"+string(rune('0'+i))), up)); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("5 requests at 100 rps took %s; limiter seems inactive", elapsed)
	}
}

// countingHoster records whether the module was asked to rebuild anything.
type countingHoster struct {
	api.FormatModule
	reindexed int
}

func (h *countingHoster) HandlePublish(context.Context, api.Feed, *http.Request, api.CoreServices) error {
	return nil
}

func (h *countingHoster) Reindex(context.Context, api.Feed, api.CoreServices) error {
	h.reindexed++
	return nil
}

// A feed that hosts nothing has no index of its own, and generating one
// anyway is not merely wasted work: the generated document is written to the
// same key the proxy cache uses, so an empty index lands on top of what the
// upstream gave us and is served as fresh until the TTL runs out. A single
// reconfiguration of a proxy feed emptied it.
//
// Supporting hosting is a property of the format; hosting is a property of
// the feed, and this is the one that decides.
func TestReindexSkipsFeedsThatHostNothing(t *testing.T) {
	publisher := NewPublisher(PublisherOptions{Store: newMemStore(time.Now), Site: "test"})

	tests := []struct {
		name string
		feed api.Feed
		want int
	}{
		{"a proxy feed", api.Feed{Name: "proxy", Upstream: "https://example.com"}, 0},
		{"a group", api.Feed{Name: "group", Group: true}, 0},
		{"a hosted feed", api.Feed{Name: "hosted", Hosted: true}, 1},
		{"one that both hosts and proxies",
			api.Feed{Name: "both", Hosted: true, Upstream: "https://example.com"}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hoster := &countingHoster{}
			if err := publisher.Reindex(t.Context(), tc.feed, hoster); err != nil {
				t.Fatalf("Reindex: %v", err)
			}
			if hoster.reindexed != tc.want {
				t.Errorf("reindexed %d times, want %d", hoster.reindexed, tc.want)
			}
		})
	}
}
