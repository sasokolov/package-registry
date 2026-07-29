// Package npm implements the FormatModule for the npm registry protocol:
//
//	GET /{pkg}                     package root (mutable metadata, SWR)
//	GET /@{scope}%2f{pkg}          scoped package root
//	GET /{pkg}/-/{tarball}         tarball (immutable artifact)
//	GET /-/package/{pkg}/dist-tags dist-tags (mutable)
//
// RewriteMetadata rewrites every dist.tarball URL to point at this registry
// while keeping dist.integrity/shasum, so clients verify the original
// upstream digests end to end.
package npm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

// metadataTTL bounds package-root freshness (SWR beyond it).
const metadataTTL = 5 * time.Minute

// searchPath is npm's search endpoint, and searchTTL bounds a proxied
// search: results move, and a stale answer to "what exists" is cheap to
// refresh.
const (
	searchPath = "-/v1/search"
	searchTTL  = time.Minute
)

func init() {
	api.RegisterFormat(Module{})
}

// Module implements api.FormatModule.
type Module struct{}

// Name implements api.FormatModule.
func (Module) Name() string { return "npm" }

// Routes implements api.FormatModule.
func (Module) Routes() []api.Route {
	return []api.Route{
		{Method: http.MethodGet, Pattern: "/*"},
		{Method: http.MethodHead, Pattern: "/*"},
	}
}

// Parse implements api.FormatModule.
func (Module) Parse(r *http.Request) (api.Intent, error) {
	// npm encodes the scope separator as %2f; use the raw path so it is not
	// confused with a path separator.
	raw := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	if raw == "" {
		return api.Intent{}, api.NotFoundf("empty path")
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return api.Intent{}, api.NotFoundf("invalid path %q", raw)
	}
	if strings.Contains(decoded, "..") {
		return api.Intent{}, api.NotFoundf("invalid path %q", decoded)
	}

	// Search: /-/v1/search?text=...
	if raw == searchPath {
		return api.Intent{
			Kind:     api.IntentSearch,
			Coord:    api.PackageCoordinate{Format: "npm", Name: "search"},
			CacheTTL: searchTTL,
			// The query is the question; without it a proxy would ask its
			// upstream for nothing at all.
			RemotePath:  searchPath,
			RemoteQuery: r.URL.RawQuery,
			ContentType: "application/json",
		}, nil
	}

	// dist-tags: /-/package/{pkg}/dist-tags
	if rest, ok := strings.CutPrefix(raw, "-/package/"); ok {
		name, tail, found := cutLast(rest, "/")
		if !found || tail != "dist-tags" {
			return api.Intent{}, api.NotFoundf("unsupported path %q", raw)
		}
		pkg, err := url.PathUnescape(name)
		if err != nil {
			return api.Intent{}, api.NotFoundf("invalid package name %q", name)
		}
		return api.Intent{
			Kind:     api.IntentMetadata,
			Coord:    api.PackageCoordinate{Format: "npm", Name: pkg},
			CacheTTL: metadataTTL,
			// Decoded form: the URL builder escapes it correctly, and npm
			// registries accept both /@scope/pkg and /@scope%2fpkg.
			RemotePath:  "-/package/" + pkg + "/dist-tags",
			ContentType: "application/json",
		}, nil
	}

	// Tarball: {pkg}/-/{file} or @scope/{pkg}/-/{file}
	if idx := strings.Index(decoded, "/-/"); idx > 0 {
		pkg := decoded[:idx]
		file := decoded[idx+3:]
		if file == "" || strings.Contains(file, "/") || !validPackageName(pkg) {
			return api.Intent{}, api.NotFoundf("invalid tarball path %q", decoded)
		}
		return api.Intent{
			Kind: api.IntentArtifact,
			Coord: api.PackageCoordinate{
				Format:  "npm",
				Name:    pkg,
				Version: versionFromTarball(pkg, file),
			},
			RemotePath:  decoded,
			ContentType: "application/octet-stream",
		}, nil
	}

	// Package root: {pkg} or @scope%2f{pkg} (also accepted unescaped).
	pkg := decoded
	if !validPackageName(pkg) {
		return api.Intent{}, api.NotFoundf("invalid package name %q", decoded)
	}
	return api.Intent{
		Kind:        api.IntentMetadata,
		Coord:       api.PackageCoordinate{Format: "npm", Name: pkg},
		CacheTTL:    metadataTTL,
		RemotePath:  pkg,
		ContentType: "application/json",
	}, nil
}

// versionFromTarball extracts the version from an attachment or tarball
// filename, and returns "" when the name is not one of this package's.
//
// A scoped package is spelled two ways and both are correct: npm uploads the
// attachment as "@scope/name-1.0.0.tgz", keeping the scope, and serves the
// tarball at a path whose last segment drops it — "name-1.0.0.tgz". Accepting
// only one of them silently produced a version of "@scope/name-1.0.0", which
// then went into a coordinate and a stored path that no client would ever ask
// for.
func versionFromTarball(pkg, file string) string {
	base := strings.TrimSuffix(file, ".tgz")
	for _, prefix := range []string{pkg + "-", shortName(pkg) + "-"} {
		if version, ok := strings.CutPrefix(base, prefix); ok && version != "" {
			return version
		}
	}
	return ""
}

// shortName is the package name without its scope, which is how npm spells it
// in a tarball's filename.
func shortName(pkg string) string {
	if _, rest, ok := strings.Cut(pkg, "/"); ok {
		return rest
	}
	return pkg
}

func validPackageName(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") || strings.Contains(name, "..") {
		return false
	}
	if strings.HasPrefix(name, "@") {
		scope, rest, ok := strings.Cut(name[1:], "/")
		return ok && scope != "" && rest != "" && !strings.Contains(rest, "/")
	}
	return !strings.Contains(name, "/")
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// RewriteMetadata implements api.FormatModule: point every dist.tarball at
// this registry, keep dist.integrity/shasum untouched (clients verify the
// upstream digest), and preserve every other field verbatim — npm package
// roots carry a lot of unspecified data that must survive.
func (Module) RewriteMetadata(feed api.Feed, body []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse npm metadata: %w", err)
	}
	versions, ok := doc["versions"].(map[string]any)
	if !ok {
		// Documents without a versions map (e.g. dist-tags) pass through.
		return body, nil
	}
	base := feedBase(feed)
	upstreamPrefix := upstreamPathPrefix(feed)
	for _, raw := range versions {
		version, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dist, ok := version["dist"].(map[string]any)
		if !ok {
			continue
		}
		tarball, ok := dist["tarball"].(string)
		if !ok || tarball == "" {
			continue
		}
		rewritten, err := rewriteTarballURL(base, upstreamPrefix, tarball)
		if err != nil {
			return nil, err
		}
		dist["tarball"] = rewritten
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode npm metadata: %w", err)
	}
	return out, nil
}

// feedBase is the public base URL of this feed; an empty site.external_url
// yields a root-relative base, which npm resolves against the registry it
// was configured with.
func feedBase(feed api.Feed) string {
	prefix := "/npm/" + feed.Name
	if feed.ExternalURL != "" {
		return strings.TrimSuffix(feed.ExternalURL, "/") + prefix
	}
	return prefix
}

// upstreamPathPrefix is the path component of the feed's upstream, e.g.
// "npm/" for http://host/npm. Registries served under a sub-path repeat it
// in their tarball URLs, and it must not end up in the registry path.
func upstreamPathPrefix(feed api.Feed) string {
	if feed.Upstream == "" {
		return ""
	}
	u, err := url.Parse(feed.Upstream)
	if err != nil {
		return ""
	}
	p := strings.Trim(u.EscapedPath(), "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// rewriteTarballURL maps an upstream tarball URL onto this registry,
// preserving the package/tarball path so Parse can map it back. The
// upstream's own path prefix is stripped: the registry mounts the feed at
// its own prefix already.
func rewriteTarballURL(base, upstreamPrefix, tarball string) (string, error) {
	u, err := url.Parse(tarball)
	if err != nil {
		return "", fmt.Errorf("parse tarball url %q: %w", tarball, err)
	}
	path := strings.TrimPrefix(u.EscapedPath(), "/")
	if path == "" {
		return "", fmt.Errorf("tarball url %q has no path", tarball)
	}
	if upstreamPrefix != "" {
		path = strings.TrimPrefix(path, upstreamPrefix)
	}
	return base + "/" + path, nil
}

// RedirectSafeIntent implements api.RedirectSafe: tarballs may be answered
// with a pre-signed redirect (npm verifies dist.integrity itself); package
// documents are rewritten by the registry and must be streamed.
func (Module) RedirectSafeIntent(intent api.Intent) bool {
	return intent.Kind == api.IntentArtifact
}
