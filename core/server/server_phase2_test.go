package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sasokolov/package-registry/core/api"
)

// synthModule exercises the Synthesizer and RootRouter capabilities.
type synthModule struct{ srvtestModule }

func (synthModule) Name() string { return "srvsynth" }

func (m synthModule) Parse(r *http.Request) (api.Intent, error) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if strings.HasSuffix(p, "/download") {
		return api.Intent{
			Kind:       api.IntentSynthetic,
			Coord:      api.PackageCoordinate{Format: "srvsynth", Name: strings.TrimSuffix(p, "/download")},
			RemotePath: p,
		}, nil
	}
	if p == "reject-me" {
		return api.Intent{}, api.NotFoundf("this path is rejected on purpose (clear client-facing body)")
	}
	return m.srvtestModule.Parse(r)
}

func (synthModule) Synthesize(_ api.Feed, intent api.Intent) (api.SyntheticResponse, error) {
	return api.SyntheticResponse{
		Status: http.StatusNoContent,
		Header: map[string]string{"X-Test-Get": "archive.tar.gz?for=" + intent.Coord.Name},
	}, nil
}

func (synthModule) RootRoutes() []api.Route {
	return []api.Route{{Method: http.MethodGet, Pattern: "/.well-known/srvsynth.json"}}
}

func (synthModule) ServeRoot(w http.ResponseWriter, _ *http.Request, feeds []api.Feed) {
	names := make([]string, 0, len(feeds))
	for _, f := range feeds {
		names = append(names, f.Name)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"feeds": names})
}

var registerSynth sync.Once

func newSynthEnv(t *testing.T) *env {
	t.Helper()
	registerSynth.Do(func() { api.RegisterFormat(synthModule{}) })
	return newEnv(t, `
  - name: sfeed
    format: srvsynth
    upstream: $UPSTREAM
    anonymous: true
`)
}

func TestSyntheticResponse(t *testing.T) {
	e := newSynthEnv(t)
	resp, _ := e.get("/srvsynth/sfeed/ns/mod/1.0.0/download", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Test-Get"); !strings.Contains(got, "ns/mod/1.0.0") {
		t.Errorf("X-Test-Get = %q", got)
	}
	if got := resp.Header.Get(api.SourceHeader); got != "local" {
		t.Errorf("%s = %q, want local", api.SourceHeader, got)
	}
}

func TestRootRouteServedWithFormatFeeds(t *testing.T) {
	e := newSynthEnv(t)
	resp, body := e.get("/.well-known/srvsynth.json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, `"sfeed"`) {
		t.Errorf("well-known body = %q, want feed list", body)
	}
}

func TestParseErrorBodyIsClientFacing(t *testing.T) {
	e := newSynthEnv(t)
	resp, body := e.get("/srvsynth/sfeed/reject-me", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(body, "rejected on purpose") {
		t.Errorf("404 body %q lacks the module's explanation", body)
	}
}
