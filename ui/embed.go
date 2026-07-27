// Package webui embeds the operator console and serves it.
//
// The console is a single-page application: one HTML document, content-hashed
// assets beside it, and client-side routing for everything else. That shape
// decides the rules here. Assets are immutable because their name contains
// their hash; the document is not, because it is what points at the current
// hashes. An unknown path under the console prefix is a route the browser
// owns, so it gets the document; an unknown path under assets/ is a genuine
// miss and gets a 404, because answering it with HTML and a 200 would turn a
// stale asset reference into a syntax error instead of a clear failure.
package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// dist is the built console. The build output is not committed, so a checkout
// without a Node toolchain still compiles: the directory holds a committed
// placeholder (which `npm run build` puts back, since Vite empties the
// directory) and the console reports itself as not built rather than
// pretending.
//
//go:embed all:dist
var dist embed.FS

// Prefix is where the console lives. It is reserved from feed names in
// core/server, so no package path can shadow it.
const Prefix = "/ui/"

// assetDir holds the content-hashed build output.
const assetDir = "assets/"

// indexFile is the document every client-side route resolves to.
const indexFile = "index.html"

// contentPolicy is what the console needs and nothing else: its own origin
// for code, styles, images and API calls. Inline style attributes are allowed
// because React writes them for the handful of one-off layout values; inline
// *scripts* are not, and the build never emits one.
const contentPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'; " +
	"object-src 'none'"

// asset is one built file, held in memory with its identity precomputed.
type asset struct {
	body        []byte
	etag        string
	contentType string
	immutable   bool
}

// Console serves the built single-page application.
type Console struct {
	files map[string]*asset
	index *asset
}

// New loads the embedded console. It never fails: a binary built without the
// console is a real state an operator can be in, and it is better to say so
// on the console's own URL than to refuse to start a registry whose actual
// job is serving packages.
func New() *Console {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return &Console{files: map[string]*asset{}}
	}
	return newFromFS(sub)
}

// newFromFS builds a console from an already-rooted build tree.
func newFromFS(sub fs.FS) *Console {
	c := &Console{files: map[string]*asset{}}
	_ = fs.WalkDir(sub, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		body, err := fs.ReadFile(sub, name)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(body)
		a := &asset{
			body:        body,
			etag:        `"` + hex.EncodeToString(sum[:16]) + `"`,
			contentType: contentTypeOf(name),
			immutable:   strings.HasPrefix(name, assetDir),
		}
		c.files[name] = a
		if name == indexFile {
			c.index = a
		}
		return nil
	})
	return c
}

// Built reports whether this binary carries a usable console.
func (c *Console) Built() bool { return c.index != nil }

// Middleware puts the console in front of the feed router: the site root
// redirects to it, its own prefix is served from memory, and everything else
// falls through untouched.
func (c *Console) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case c.Built() && (p == "/" || p == "/index.html"):
			http.Redirect(w, r, Prefix, http.StatusFound)
		case p == strings.TrimSuffix(Prefix, "/"):
			http.Redirect(w, r, Prefix, http.StatusFound)
		case strings.HasPrefix(p, Prefix):
			c.serve(w, r, strings.TrimPrefix(p, Prefix))
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// serve answers one console request. rel is the path below Prefix.
func (c *Console) serve(w http.ResponseWriter, r *http.Request, rel string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "the console is read-only", http.StatusMethodNotAllowed)
		return
	}
	if !c.Built() {
		http.Error(w,
			"this binary was built without the web console; build it with `make ui`",
			http.StatusServiceUnavailable)
		return
	}

	// A path with a .. segment cannot name a build output, and normalising it
	// away silently would let it name one.
	if rel != path.Clean(rel) && rel != "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if a, ok := c.files[rel]; ok && rel != "" {
		c.write(w, r, rel, a)
		return
	}
	if strings.HasPrefix(rel, assetDir) {
		// A missing hashed asset means the document and the build disagree.
		// Say so plainly instead of returning the document.
		http.Error(w, "no such asset", http.StatusNotFound)
		return
	}
	c.write(w, r, indexFile, c.index)
}

// write emits one asset with the caching and safety headers its kind earns.
func (c *Console) write(w http.ResponseWriter, r *http.Request, name string, a *asset) {
	h := w.Header()
	h.Set("Content-Type", a.contentType)
	h.Set("ETag", a.etag)
	h.Set("X-Content-Type-Options", "nosniff")
	if a.immutable {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// The document names the current asset hashes, so it must be
		// revalidated or a deploy would be invisible to a warm browser.
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Security-Policy", contentPolicy)
		h.Set("Referrer-Policy", "same-origin")
	}
	// The zero time keeps Last-Modified out of the response: the ETag is the
	// whole truth about this file, and an embedded file has no useful mtime.
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(a.body))
}

// contentTypeOf types a build output by extension, with the few defaults Go
// does not carry on every platform spelled out.
func contentTypeOf(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}
