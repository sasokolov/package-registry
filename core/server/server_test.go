package server

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/config"

	_ "github.com/sasokolov/package-registry/modules/storage/fs" // storage under test
	_ "github.com/sasokolov/package-registry/policies/allowlist" // policy under test
)

// srvtestModule mirrors the conformance echo module but registers under its
// own name to keep this package self-contained.
type srvtestModule struct{}

func (srvtestModule) Name() string { return "srvtest" }
func (srvtestModule) Routes() []api.Route {
	return []api.Route{{Method: http.MethodGet, Pattern: "/*"}}
}
func (srvtestModule) Parse(r *http.Request) (api.Intent, error) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		return api.Intent{}, api.NotFoundf("empty path")
	}
	return api.Intent{
		Kind:       api.IntentArtifact,
		Coord:      api.PackageCoordinate{Format: "srvtest", Name: p},
		RemotePath: p,
	}, nil
}
func (srvtestModule) RewriteMetadata(_ api.Feed, b []byte) ([]byte, error) { return b, nil }

var registerModule sync.Once

type env struct {
	t        *testing.T
	upstream *httptest.Server
	registry *httptest.Server
	manager  *config.Manager
	cfgPath  string
}

func newEnv(t *testing.T, feedYAML string) *env {
	t.Helper()
	registerModule.Do(func() { api.RegisterFormat(srvtestModule{}) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/present.txt" || r.URL.Path == "/allowed/thing.txt" || r.URL.Path == "/blocked/thing.txt" {
			_, _ = io.WriteString(w, "content of "+r.URL.Path)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	writeCfg := fmt.Sprintf(`
storage:
  type: fs
  fs: {path: %s}
feeds:
%s`, t.TempDir(), strings.ReplaceAll(feedYAML, "$UPSTREAM", upstream.URL))
	if err := os.WriteFile(cfgPath, []byte(writeCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	manager, err := config.NewManager(cfgPath, logger, ValidateConfig)
	if err != nil {
		t.Fatal(err)
	}
	store, err := api.NewStorage("fs", manager.Current().Storage.Options())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(t.Context(), Options{Logger: logger, Store: store, Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	registry := httptest.NewServer(srv.Handler())
	t.Cleanup(registry.Close)
	return &env{t: t, upstream: upstream, registry: registry, manager: manager, cfgPath: cfgPath}
}

func (e *env) get(path string, header http.Header) (*http.Response, string) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodGet, e.registry.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		e.t.Fatal(err)
	}
	return resp, string(body)
}

func TestAnonymousFeedServesAndCaches(t *testing.T) {
	e := newEnv(t, `
  - name: pub
    format: srvtest
    upstream: $UPSTREAM
    anonymous: true
`)

	resp, body := e.get("/srvtest/pub/present.txt", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get(api.SourceHeader); got != "upstream" {
		t.Errorf("first %s = %q, want upstream", api.SourceHeader, got)
	}
	if body != "content of /present.txt" {
		t.Errorf("body = %q", body)
	}

	e.upstream.Close() // registry must now serve from cache
	resp, body = e.get("/srvtest/pub/present.txt", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cached status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get(api.SourceHeader); got != "cache" {
		t.Errorf("second %s = %q, want cache", api.SourceHeader, got)
	}
	if body != "content of /present.txt" {
		t.Errorf("cached body = %q", body)
	}
}

func TestAuthRequiredFeed(t *testing.T) {
	e := newEnv(t, `
  - name: priv
    format: srvtest
    upstream: $UPSTREAM
    anonymous: false
`)

	resp, _ := e.get("/srvtest/priv/present.txt", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("401 without WWW-Authenticate")
	}

	// Garbage credentials are rejected even though the DB is disabled.
	resp, _ = e.get("/srvtest/priv/present.txt", http.Header{"Authorization": {"Bearer reg_garbage"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token status = %d, want 401", resp.StatusCode)
	}
}

func TestAllowlistDeniesWith403(t *testing.T) {
	e := newEnv(t, `
  - name: guarded
    format: srvtest
    upstream: $UPSTREAM
    anonymous: true
    policies:
      - name: allowlist
        config:
          allow: ["allowed/*"]
`)

	resp, _ := e.get("/srvtest/guarded/allowed/thing.txt", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowed coordinate status = %d", resp.StatusCode)
	}
	resp, body := e.get("/srvtest/guarded/blocked/thing.txt", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("blocked coordinate status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(body, "allowlist") {
		t.Errorf("403 body %q lacks the policy reason", body)
	}
}

func TestUnknownFeedAndUpstream404(t *testing.T) {
	e := newEnv(t, `
  - name: pub
    format: srvtest
    upstream: $UPSTREAM
    anonymous: true
`)
	if resp, _ := e.get("/srvtest/other/present.txt", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown feed status = %d, want 404", resp.StatusCode)
	}
	if resp, _ := e.get("/srvtest/pub/missing.txt", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing upstream file status = %d, want 404", resp.StatusCode)
	}
}

func TestHotReloadAddsFeed(t *testing.T) {
	e := newEnv(t, `
  - name: pub
    format: srvtest
    upstream: $UPSTREAM
    anonymous: true
`)
	if resp, _ := e.get("/srvtest/extra/present.txt", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("feed exists before reload, status = %d", resp.StatusCode)
	}

	raw, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	extra := fmt.Sprintf(`  - name: extra
    format: srvtest
    upstream: %s
    anonymous: true
`, e.upstream.URL)
	if err := os.WriteFile(e.cfgPath, append(raw, []byte(extra)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.manager.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, _ := e.get("/srvtest/extra/present.txt", nil)
		if resp.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new feed still %d after reload", resp.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
