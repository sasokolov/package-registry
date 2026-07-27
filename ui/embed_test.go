package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// built is a stand-in for a real build: one document and one hashed asset.
// The test does not depend on Node having run, which is the point — the
// serving rules are what is under test, not the bundler.
func built() *Console {
	return newFromFS(fstest.MapFS{
		"index.html":              {Data: []byte(`<!doctype html><div id="root"></div>`)},
		"assets/index-abc123.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc123.css": {Data: []byte(".a{}")},
	})
}

func do(t *testing.T, h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func passthrough() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
}

func TestConsoleRouting(t *testing.T) {
	h := built().Middleware(passthrough())

	tests := []struct {
		name        string
		target      string
		wantStatus  int
		wantBodyHas string
		wantHeader  map[string]string
	}{
		{
			name: "root redirects to the console", target: "/",
			wantStatus: http.StatusFound,
			wantHeader: map[string]string{"Location": "/ui/"},
		},
		{
			name: "bare prefix redirects to the trailing slash", target: "/ui",
			wantStatus: http.StatusFound,
			wantHeader: map[string]string{"Location": "/ui/"},
		},
		{
			name: "the console root serves the document", target: "/ui/",
			wantStatus: http.StatusOK, wantBodyHas: `id="root"`,
			wantHeader: map[string]string{"Cache-Control": "no-cache"},
		},
		{
			name: "a deep link serves the document", target: "/ui/feeds/maven-central",
			wantStatus: http.StatusOK, wantBodyHas: `id="root"`,
		},
		{
			name: "hashed assets are immutable", target: "/ui/assets/index-abc123.js",
			wantStatus: http.StatusOK, wantBodyHas: "console.log",
			wantHeader: map[string]string{
				"Cache-Control": "public, max-age=31536000, immutable",
				"Content-Type":  "text/javascript; charset=utf-8",
			},
		},
		{
			name:   "a missing asset is a miss, not the document",
			target: "/ui/assets/index-gone.js", wantStatus: http.StatusNotFound,
		},
		{
			name: "a feed path is not the console's business", target: "/maven-central/junit/junit/4.13/junit-4.13.jar",
			wantStatus: http.StatusTeapot,
		},
		{
			name: "the console is read-only", target: "/ui/",
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			method := http.MethodGet
			if tc.wantStatus == http.StatusMethodNotAllowed {
				method = http.MethodPost
			}
			w := do(t, h, method, tc.target, nil)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if tc.wantBodyHas != "" && !strings.Contains(w.Body.String(), tc.wantBodyHas) {
				t.Fatalf("body %q does not contain %q", w.Body.String(), tc.wantBodyHas)
			}
			for k, v := range tc.wantHeader {
				if got := w.Header().Get(k); got != v {
					t.Fatalf("header %s = %q, want %q", k, got, v)
				}
			}
		})
	}
}

func TestDocumentCarriesAContentPolicy(t *testing.T) {
	w := do(t, built().Middleware(passthrough()), http.MethodGet, "/ui/", nil)
	if got := w.Header().Get("Content-Security-Policy"); got != contentPolicy {
		t.Fatalf("Content-Security-Policy = %q, want the console policy", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
}

func TestAssetsRevalidateWithETag(t *testing.T) {
	h := built().Middleware(passthrough())
	first := do(t, h, http.MethodGet, "/ui/assets/index-abc123.css", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on an asset")
	}
	second := do(t, h, http.MethodGet, "/ui/assets/index-abc123.css",
		map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 carried %d bytes of body", second.Body.Len())
	}
}

// A binary without a build must say so on the console's own URL rather than
// serving nothing, and must leave the rest of the site alone.
func TestUnbuiltConsoleExplainsItself(t *testing.T) {
	empty := newFromFS(fstest.MapFS{})
	if empty.Built() {
		t.Fatal("an empty build tree reported itself as built")
	}
	h := empty.Middleware(passthrough())

	if w := do(t, h, http.MethodGet, "/ui/", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("console status = %d, want 503", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/", nil); w.Code != http.StatusTeapot {
		t.Fatalf("root status = %d, want the feed router to answer", w.Code)
	}
}

func TestTraversalIsRefused(t *testing.T) {
	h := built().Middleware(passthrough())
	// A dot-dot segment cannot name a build output. Cleaning it away first
	// and then looking the result up is how a traversal becomes a read, so
	// the path is refused instead.
	w := do(t, h, http.MethodGet, "/ui/assets/../index.html", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
