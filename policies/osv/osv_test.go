package osv

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
)

// fakeVerdicts is an in-memory PolicyServices.
type fakeVerdicts struct {
	mu     sync.Mutex
	values map[string]string
	at     map[string]time.Time
	fail   bool
}

func newFakeVerdicts() *fakeVerdicts {
	return &fakeVerdicts{values: map[string]string{}, at: map[string]time.Time{}}
}

func (f *fakeVerdicts) GetVerdict(_ context.Context, ns, key string) (string, time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return "", time.Time{}, false, io.ErrUnexpectedEOF
	}
	v, ok := f.values[ns+"/"+key]
	return v, f.at[ns+"/"+key], ok, nil
}

func (f *fakeVerdicts) PutVerdict(_ context.Context, ns, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return io.ErrUnexpectedEOF
	}
	f.values[ns+"/"+key] = value
	f.at[ns+"/"+key] = time.Now()
	return nil
}

func (f *fakeVerdicts) Logger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// fakeOSV serves canned OSV responses and counts queries.
type fakeOSV struct {
	server *httptest.Server
	calls  atomic.Int64
	vulns  map[string][]string // "pkg@version" -> advisory ids
	down   atomic.Bool
}

func newFakeOSV(t *testing.T, vulns map[string][]string) *fakeOSV {
	f := &fakeOSV{vulns: vulns}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if f.down.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		var req osvRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ids := f.vulns[req.Package.Name+"@"+req.Version]
		resp := osvResponse{}
		for _, id := range ids {
			resp.Vulns = append(resp.Vulns, struct {
				ID        string `json:"id"`
				Withdrawn string `json:"withdrawn"`
			}{ID: id})
		}
		// One withdrawn advisory must never count as a finding.
		resp.Vulns = append(resp.Vulns, struct {
			ID        string `json:"id"`
			Withdrawn string `json:"withdrawn"`
		}{ID: "WITHDRAWN-1", Withdrawn: "2026-01-01T00:00:00Z"})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func newPolicy(t *testing.T, f *fakeOSV, deps api.PolicyServices, options map[string]any) *Policy {
	t.Helper()
	if options == nil {
		options = map[string]any{}
	}
	options["api_url"] = f.server.URL
	p, err := New(options, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pol := p.(*Policy)
	pol.client = f.server.Client()
	return pol
}

func vulnerableArtifact() api.Artifact {
	return api.Artifact{
		Coord:    api.PackageCoordinate{Format: "maven", Name: "com.example:vuln", Version: "1.0"},
		Metadata: map[string]string{api.MetaEcosystem: "Maven"},
	}
}

func cleanArtifact() api.Artifact {
	return api.Artifact{
		Coord:    api.PackageCoordinate{Format: "maven", Name: "com.example:safe", Version: "1.0"},
		Metadata: map[string]string{api.MetaEcosystem: "Maven"},
	}
}

func TestOSVEnforceDeniesVulnerable(t *testing.T) {
	f := newFakeOSV(t, map[string][]string{"com.example:vuln@1.0": {"GHSA-xxxx", "CVE-2026-1"}})
	p := newPolicy(t, f, newFakeVerdicts(), map[string]any{"mode": "enforce"})
	ctx := t.Context()

	d := p.OnServe(ctx, api.Anonymous(), vulnerableArtifact())
	if d.Allow {
		t.Fatal("vulnerable artifact allowed in enforce mode")
	}
	if d.Code != DenyCode {
		t.Errorf("code = %q", d.Code)
	}
	if d.Reason == "" || !contains(d.Reason, "CVE-2026-1") {
		t.Errorf("reason %q does not name the advisories", d.Reason)
	}

	if d := p.OnServe(ctx, api.Anonymous(), cleanArtifact()); !d.Allow {
		t.Errorf("clean artifact denied: %+v", d)
	}
}

func TestOSVWarnModeAllows(t *testing.T) {
	f := newFakeOSV(t, map[string][]string{"com.example:vuln@1.0": {"GHSA-xxxx"}})
	p := newPolicy(t, f, newFakeVerdicts(), map[string]any{"mode": "warn"})
	if d := p.OnServe(t.Context(), api.Anonymous(), vulnerableArtifact()); !d.Allow {
		t.Fatal("warn mode denied a vulnerable artifact")
	}
}

func TestOSVCaching(t *testing.T) {
	f := newFakeOSV(t, map[string][]string{"com.example:vuln@1.0": {"GHSA-xxxx"}})
	shared := newFakeVerdicts()
	p := newPolicy(t, f, shared, map[string]any{"mode": "enforce"})
	ctx := t.Context()

	for i := 0; i < 3; i++ {
		p.OnServe(ctx, api.Anonymous(), vulnerableArtifact())
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("OSV queried %d times, want 1 (per-process cache)", got)
	}

	// A fresh replica reuses the shared verdict instead of querying again.
	p2 := newPolicy(t, f, shared, map[string]any{"mode": "enforce"})
	if d := p2.OnServe(ctx, api.Anonymous(), vulnerableArtifact()); d.Allow {
		t.Error("second replica allowed a vulnerable artifact")
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("OSV queried %d times, want still 1 (shared cache)", got)
	}
}

func TestOSVFailOpenAndClosed(t *testing.T) {
	f := newFakeOSV(t, nil)
	f.down.Store(true)
	ctx := t.Context()

	open := newPolicy(t, f, newFakeVerdicts(), map[string]any{"mode": "enforce", "fail_open": true})
	if d := open.OnServe(ctx, api.Anonymous(), vulnerableArtifact()); !d.Allow {
		t.Error("fail-open policy denied while OSV is down")
	}

	closed := newPolicy(t, f, newFakeVerdicts(), map[string]any{"mode": "enforce", "fail_open": false})
	if d := closed.OnServe(ctx, api.Anonymous(), vulnerableArtifact()); d.Allow {
		t.Error("fail-closed policy allowed while OSV is down")
	}
}

func TestOSVVerdictStoreOutageDoesNotBlock(t *testing.T) {
	f := newFakeOSV(t, map[string][]string{"com.example:vuln@1.0": {"GHSA-xxxx"}})
	broken := newFakeVerdicts()
	broken.fail = true
	p := newPolicy(t, f, broken, map[string]any{"mode": "enforce"})

	// The shared cache is unavailable: the policy still queries OSV and
	// reaches a verdict.
	if d := p.OnServe(t.Context(), api.Anonymous(), vulnerableArtifact()); d.Allow {
		t.Error("vulnerable artifact allowed although OSV answered")
	}
}

func TestOSVSkipsIncompleteCoordinates(t *testing.T) {
	f := newFakeOSV(t, nil)
	p := newPolicy(t, f, newFakeVerdicts(), map[string]any{"mode": "enforce"})
	ctx := t.Context()

	// No ecosystem, no version: nothing to ask OSV about.
	if d := p.OnServe(ctx, api.Anonymous(), api.Artifact{
		Coord: api.PackageCoordinate{Name: "x", Version: "1"},
	}); !d.Allow {
		t.Error("artifact without ecosystem denied")
	}
	if d := p.OnResolve(ctx, api.Anonymous(), api.PackageCoordinate{Format: "maven", Name: "x"}); !d.Allow {
		t.Error("version-less coordinate denied")
	}
	if f.calls.Load() != 0 {
		t.Errorf("OSV queried %d times for incomplete coordinates", f.calls.Load())
	}
}

func TestOSVBadOptions(t *testing.T) {
	for i, opts := range []map[string]any{
		{"mode": "block"},
		{"fail_open": "yes"},
		{"cache_ttl": 42},
		{"cache_ttl": "never"},
	} {
		if _, err := New(opts, nil); err == nil {
			t.Errorf("case %d: bad options accepted: %v", i, opts)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
