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
	"testing"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/config"
)

// groupEnv is newEnv with two upstreams, so a group can be built over two
// proxy members that genuinely hold different things.
type groupEnv struct {
	*env
	second *httptest.Server
}

func newGroupEnv(t *testing.T, feedYAML string) *groupEnv {
	t.Helper()
	registerModule.Do(func() { api.RegisterFormat(srvtestModule{}) })

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/only-first.txt":
			_, _ = io.WriteString(w, "from the first member")
		case "/both.txt":
			_, _ = io.WriteString(w, "first member's copy")
		case "/pkg/index":
			_, _ = io.WriteString(w, "1.0.0\n2.0.0\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(first.Close)

	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/only-second.txt":
			_, _ = io.WriteString(w, "from the second member")
		case "/both.txt":
			_, _ = io.WriteString(w, "second member's copy")
		case "/pkg/index":
			_, _ = io.WriteString(w, "2.0.0\n3.0.0\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(second.Close)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	body := strings.ReplaceAll(feedYAML, "$FIRST", first.URL)
	body = strings.ReplaceAll(body, "$SECOND", second.URL)
	writeCfg := fmt.Sprintf(`
storage:
  type: fs
  fs: {path: %s}
feeds:
%s`, t.TempDir(), body)
	if err := os.WriteFile(cfgPath, []byte(writeCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	manager, err := config.NewManager(cfgPath, logger, ValidateConfig)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	store, err := api.NewStorage("fs", manager.Current().Storage.Options())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(t.Context(), Options{Logger: logger, Store: store, Manager: manager})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	registry := httptest.NewServer(srv.Handler())
	t.Cleanup(registry.Close)

	return &groupEnv{
		env:    &env{t: t, upstream: first, registry: registry, manager: manager, cfgPath: cfgPath},
		second: second,
	}
}

const twoProxyGroup = `
  - name: alpha
    format: srvtest
    upstream: $FIRST
    anonymous: true
  - name: beta
    format: srvtest
    upstream: $SECOND
    anonymous: true
  - name: all
    format: srvtest
    anonymous: true
    members: [alpha, beta]
`

// The point of a group: one URL, and what any member holds comes back.
func TestGroupServesFromEveryMember(t *testing.T) {
	e := newGroupEnv(t, twoProxyGroup)

	tests := []struct {
		path, wantBody, wantMember string
	}{
		{"/srvtest/all/only-first.txt", "from the first member", "alpha"},
		{"/srvtest/all/only-second.txt", "from the second member", "beta"},
	}
	for _, tc := range tests {
		resp, body := e.get(tc.path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, body %q", tc.path, resp.StatusCode, body)
		}
		if body != tc.wantBody {
			t.Errorf("%s: body = %q, want %q", tc.path, body, tc.wantBody)
		}
		if got := resp.Header.Get(api.GroupMemberHeader); got != tc.wantMember {
			t.Errorf("%s: %s = %q, want %q", tc.path, api.GroupMemberHeader, got, tc.wantMember)
		}
	}
}

// Order decides who wins a coordinate both members hold — that is what makes
// "internal first" a defence rather than a coin toss.
func TestGroupOrderDecidesTheWinner(t *testing.T) {
	e := newGroupEnv(t, twoProxyGroup)
	resp, body := e.get("/srvtest/all/both.txt", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %q", resp.StatusCode, body)
	}
	if body != "first member's copy" {
		t.Errorf("body = %q, want the first member's", body)
	}
	if got := resp.Header.Get(api.GroupMemberHeader); got != "alpha" {
		t.Errorf("%s = %q, want alpha", api.GroupMemberHeader, got)
	}
}

// The index is merged, not answered by the first member — otherwise the
// group silently hides half of what it holds.
func TestGroupMergesTheIndex(t *testing.T) {
	e := newGroupEnv(t, twoProxyGroup)
	resp, body := e.get("/srvtest/all/pkg/index", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %q", resp.StatusCode, body)
	}
	if want := "1.0.0\n2.0.0\n3.0.0\n"; body != want {
		t.Fatalf("merged index = %q, want %q", body, want)
	}
	if got := resp.Header.Get(api.GroupMergedHeader); got != "alpha,beta" {
		t.Errorf("%s = %q, want alpha,beta", api.GroupMergedHeader, got)
	}
	// The merged document was produced here, from what this site had.
	if got := resp.Header.Get(api.SourceHeader); got != string(api.SourceLocal) {
		t.Errorf("%s = %q, want local", api.SourceHeader, got)
	}
}

// Nothing anywhere is a 404, and it says which group looked.
func TestGroupMissEverywhereIs404(t *testing.T) {
	e := newGroupEnv(t, twoProxyGroup)
	resp, body := e.get("/srvtest/all/nowhere.txt", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, body %q", resp.StatusCode, body)
	}
	if !strings.Contains(body, "all") {
		t.Errorf("the 404 does not name the group: %q", body)
	}
}

// A member the caller may not read is skipped, not exposed: a group can
// never widen access to what it contains.
func TestGroupDoesNotExposeAMemberTheCallerCannotRead(t *testing.T) {
	e := newGroupEnv(t, `
  - name: alpha
    format: srvtest
    upstream: $FIRST
    anonymous: false
  - name: beta
    format: srvtest
    upstream: $SECOND
    anonymous: true
  - name: all
    format: srvtest
    anonymous: true
    members: [alpha, beta]
`)

	// The private member holds it; an anonymous caller must not get it
	// through the group.
	resp, body := e.get("/srvtest/all/only-first.txt", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a private member leaked through the group: status %d, body %q", resp.StatusCode, body)
	}
	// And asking that member directly is refused, not silently empty.
	resp, _ = e.get("/srvtest/alpha/only-first.txt", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("direct access to the private feed = %d, want 401", resp.StatusCode)
	}
	// The public member still works through the group.
	resp, body = e.get("/srvtest/all/only-second.txt", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public member through the group: status %d, body %q", resp.StatusCode, body)
	}
}

// A merged index must not contain what the caller may not see either.
func TestGroupMergeSkipsUnreadableMembers(t *testing.T) {
	e := newGroupEnv(t, `
  - name: alpha
    format: srvtest
    upstream: $FIRST
    anonymous: false
  - name: beta
    format: srvtest
    upstream: $SECOND
    anonymous: true
  - name: all
    format: srvtest
    anonymous: true
    members: [alpha, beta]
`)
	resp, body := e.get("/srvtest/all/pkg/index", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %q", resp.StatusCode, body)
	}
	if strings.Contains(body, "1.0.0") {
		t.Errorf("the merged index leaked a private member's versions: %q", body)
	}
	if got := resp.Header.Get(api.GroupMergedHeader); got != "beta" {
		t.Errorf("%s = %q, want beta alone", api.GroupMergedHeader, got)
	}
}

// A group is read-only. The refusal names where to publish instead, because
// an operator who guessed wrong ends up publishing into a proxy.
func TestGroupRefusesPublish(t *testing.T) {
	e := newGroupEnv(t, twoProxyGroup)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
		e.registry.URL+"/srvtest/all/thing.txt", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("publish to a group = %d, want 405; body %q", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "read-only") {
		t.Errorf("the refusal does not explain itself: %q", body)
	}
	// No member of this group hosts, and saying so beats a generic 405.
	if !strings.Contains(string(body), "No member of all accepts publishing") {
		t.Errorf("the refusal does not say there is nowhere to publish: %q", body)
	}
}

// When a member does host, the refusal names it: that is the difference
// between a dead end and an instruction.
func TestGroupPublishRefusalNamesTheHostedMembers(t *testing.T) {
	s := &Server{}
	tests := []struct {
		name    string
		members []*feedRuntime
		want    string
	}{
		{
			name: "one hosted member",
			members: []*feedRuntime{
				{feed: api.Feed{Name: "proxy"}},
				{feed: api.Feed{Name: "releases"}, hosted: true},
			},
			want: "publish to a hosted feed: releases",
		},
		{
			name: "several",
			members: []*feedRuntime{
				{feed: api.Feed{Name: "releases"}, hosted: true},
				{feed: api.Feed{Name: "snapshots"}, hosted: true},
			},
			want: "one of: releases, snapshots",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gr := &feedRuntime{feed: api.Feed{Name: "all"}, members: tc.members}
			w := httptest.NewRecorder()
			s.groupPublishHandler(gr)(w, httptest.NewRequest(http.MethodPut, "/thing", nil))

			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", w.Code)
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body = %q, want it to contain %q", w.Body.String(), tc.want)
			}
			if got := w.Header().Get("Allow"); got != "GET, HEAD" {
				t.Errorf("Allow = %q", got)
			}
		})
	}
}

// Nesting is flattened once, in order, without repeats.
func TestNestedGroupsFlattenInOrder(t *testing.T) {
	e := newGroupEnv(t, `
  - name: alpha
    format: srvtest
    upstream: $FIRST
    anonymous: true
  - name: beta
    format: srvtest
    upstream: $SECOND
    anonymous: true
  - name: inner
    format: srvtest
    anonymous: true
    members: [alpha]
  - name: outer
    format: srvtest
    anonymous: true
    members: [inner, beta]
`)
	resp, body := e.get("/srvtest/outer/pkg/index", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get(api.GroupMergedHeader); got != "alpha,beta" {
		t.Errorf("%s = %q, want the flattened leaves alpha,beta", api.GroupMergedHeader, got)
	}
}
