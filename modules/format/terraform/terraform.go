// Package terraform implements the FormatModule for the Terraform Module
// Registry protocol v1 (proxy):
//
//	GET /.well-known/terraform.json                        (server root)
//	GET /v1/modules/{ns}/{name}/{provider}/versions        (mutable, SWR)
//	GET /v1/modules/{ns}/{name}/{provider}/{ver}/download  (synthetic 204)
//	GET /v1/modules/{ns}/{name}/{provider}/{ver}/archive.tar.gz
//
// The download endpoint is answered locally with an X-Terraform-Get header
// pointing at the registry's own archive path — never at the upstream
// (PLAN Phase 2). The archive is an indirect artifact: the pipeline asks
// the upstream download endpoint, follows its X-Terraform-Get and caches
// the module archive as a content-addressed blob.
package terraform

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
)

// versionsTTL bounds the versions document freshness (SWR beyond it).
const versionsTTL = 5 * time.Minute

// archiveFile is the registry-invented artifact name; the .tar.gz suffix
// tells terraform's go-getter how to unpack it.
const archiveFile = "archive.tar.gz"

func init() {
	api.RegisterFormat(Module{})
}

// Module implements api.FormatModule.
type Module struct{}

// Name implements api.FormatModule.
func (Module) Name() string { return "terraform" }

// Routes implements api.FormatModule.
func (Module) Routes() []api.Route {
	return []api.Route{
		{Method: http.MethodGet, Pattern: "/*"},
		{Method: http.MethodHead, Pattern: "/*"},
	}
}

// Parse implements api.FormatModule.
func (Module) Parse(r *http.Request) (api.Intent, error) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	segs := strings.Split(p, "/")
	if len(segs) < 2 || segs[0] != "v1" || segs[1] != "modules" {
		return api.Intent{}, api.NotFoundf("not a module registry path: %q", p)
	}
	rest := segs[2:]
	for _, s := range rest {
		if s == "" || s == "." || s == ".." {
			return api.Intent{}, api.NotFoundf("invalid path %q", p)
		}
	}

	switch {
	case len(rest) == 4 && rest[3] == "versions":
		return api.Intent{
			Kind:        api.IntentMetadata,
			Coord:       coord(rest, ""),
			CacheTTL:    versionsTTL,
			RemotePath:  p,
			ContentType: "application/json",
		}, nil
	case len(rest) == 5 && rest[4] == "download":
		return api.Intent{
			Kind:       api.IntentSynthetic,
			Coord:      coord(rest, rest[3]),
			RemotePath: p,
		}, nil
	case len(rest) == 5 && rest[4] == archiveFile:
		return api.Intent{
			Kind:        api.IntentArtifact,
			Coord:       coord(rest, rest[3]),
			RemotePath:  archiveIntentPath(p),
			Indirect:    true,
			ContentType: "application/gzip",
		}, nil
	default:
		return api.Intent{}, api.NotFoundf("unsupported module registry path: %q", p)
	}
}

// archiveIntentPath is what an archive request resolves to: the upstream's
// download endpoint, which is the indirection that names the real archive.
//
// It is also the cache and manifest key, so a hosted feed must publish under
// exactly this path — a request resolves to one path regardless of which
// feed answers it, and storing an archive anywhere else means storing an
// archive nobody can download.
func archiveIntentPath(requestPath string) string {
	return strings.TrimSuffix(requestPath, archiveFile) + "download"
}

func coord(rest []string, version string) api.PackageCoordinate {
	return api.PackageCoordinate{
		Format:  "terraform",
		Name:    rest[0] + "/" + rest[1] + "/" + rest[2],
		Version: version,
	}
}

// RewriteMetadata implements api.FormatModule: the versions document
// carries no URLs, it is served verbatim.
func (Module) RewriteMetadata(_ api.Feed, body []byte) ([]byte, error) {
	return body, nil
}

// Synthesize implements api.Synthesizer: the download endpoint answers with
// 204 + X-Terraform-Get pointing at the registry's own archive path.
//
// A bare relative location ("archive.tar.gz") is rejected by Terraform
// ("cannot detect a supported module source type"); the "./"-prefixed form
// is protocol-correct and is resolved against the download URL, so it works
// behind any hostname without extra configuration. An absolute URL built
// from site.external_url is used when configured, since it also survives
// clients that rewrite the request path.
func (Module) Synthesize(feed api.Feed, intent api.Intent) (api.SyntheticResponse, error) {
	if intent.Kind != api.IntentSynthetic || !strings.HasSuffix(intent.RemotePath, "/download") {
		return api.SyntheticResponse{}, api.NotFoundf("unexpected synthetic intent %q", intent.RemotePath)
	}
	location := "./" + archiveFile
	if feed.ExternalURL != "" {
		location = feed.ExternalURL + "/terraform/" + feed.Name + "/" +
			strings.TrimSuffix(intent.RemotePath, "download") + archiveFile
	}
	return api.SyntheticResponse{
		Status: http.StatusNoContent,
		Header: map[string]string{"X-Terraform-Get": location},
	}, nil
}

// ValidateFeeds implements api.FeedSetValidator: root-level service
// discovery names exactly one module registry per site, so the site has to
// be unambiguous about which one that is.
//
// One feed is the simple case. More than one is allowed only through a
// group: the group is what discovery points at, and its members exist to be
// combined rather than to be reached directly — which is the only way a
// site can serve both its own modules and a proxied registry, since a
// Terraform module source names a host, not a path.
func (Module) ValidateFeeds(feeds []api.Feed) error {
	if len(feeds) <= 1 {
		return nil
	}
	groups := make([]string, 0, 1)
	for _, f := range feeds {
		if f.Group {
			groups = append(groups, f.Name)
		}
	}
	sort.Strings(groups)
	switch len(groups) {
	case 1:
		return nil
	case 0:
		return fmt.Errorf(
			"terraform service discovery at /.well-known/terraform.json names one registry, "+
				"but %d terraform feeds are configured (%s): combine them into a group, "+
				"which is what discovery will then point at",
			len(feeds), strings.Join(names(feeds), ", "))
	default:
		return fmt.Errorf(
			"terraform service discovery names one registry, but %d groups are configured (%s)",
			len(groups), strings.Join(groups, ", "))
	}
}

func names(feeds []api.Feed) []string {
	out := make([]string, 0, len(feeds))
	for _, f := range feeds {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

// ResolveIndirect implements api.IndirectResolver: extract the module
// archive location from the upstream download response.
//
// Only HTTP(S) archive locations can be proxied. Public
// registry.terraform.io modules return go-getter VCS sources
// (git::https://github.com/...) — fetching those is a git operation, not an
// HTTP download, and is out of scope for a pull-through cache; such modules
// yield a clear 502.
func (Module) ResolveIndirect(_ api.Feed, _ api.Intent, status int, header map[string][]string, _ []byte) (api.IndirectTarget, error) {
	if status != http.StatusNoContent && status != http.StatusOK {
		return api.IndirectTarget{}, fmt.Errorf("upstream download endpoint returned status %d", status)
	}
	vals := header[http.CanonicalHeaderKey("X-Terraform-Get")]
	if len(vals) == 0 || vals[0] == "" {
		return api.IndirectTarget{}, errors.New("upstream download response lacks X-Terraform-Get")
	}
	loc := vals[0]
	if strings.Contains(loc, "::") || strings.HasPrefix(loc, "git@") {
		return api.IndirectTarget{}, fmt.Errorf("module archive location %q is a VCS source, not an HTTP archive; such upstream modules cannot be proxied: %w",
			loc, api.ErrUpstreamUnavailable)
	}
	return splitGetterURL(loc)
}

// splitGetterURL separates go-getter control parameters from the plain
// download URL: "checksum=<algo>:<hex>" becomes the expected checksum
// (invariant 5), and the unsupported subdirectory syntax is rejected
// outright rather than silently caching the wrong bytes.
func splitGetterURL(loc string) (api.IndirectTarget, error) {
	u, err := url.Parse(loc)
	if err != nil {
		return api.IndirectTarget{}, fmt.Errorf("invalid module archive location %q: %w", loc, err)
	}
	// go-getter's "//subdir" selects a directory inside the archive; the
	// double slash appears in the path, after the scheme and host.
	if strings.Contains(u.Path, "//") {
		return api.IndirectTarget{}, fmt.Errorf("module archive location %q uses go-getter subdirectory syntax, which cannot be proxied: %w",
			loc, api.ErrUpstreamUnavailable)
	}
	q := u.Query()
	raw := q.Get("checksum")
	if raw == "" {
		return api.IndirectTarget{Location: loc}, nil
	}
	q.Del("checksum")
	u.RawQuery = q.Encode()

	algo, hexDigest, ok := strings.Cut(raw, ":")
	if !ok {
		return api.IndirectTarget{}, fmt.Errorf("unsupported checksum parameter %q in module archive location", raw)
	}
	algo = strings.ToLower(algo)
	switch algo {
	case "md5", "sha1", "sha256", "sha512":
	default:
		return api.IndirectTarget{}, fmt.Errorf("unsupported checksum algorithm %q in module archive location", algo)
	}
	return api.IndirectTarget{
		Location: u.String(),
		Checksum: api.Checksum{Algo: algo, Hex: strings.ToLower(hexDigest)},
	}, nil
}

// RootRoutes implements api.RootRouter: terraform requires service
// discovery at the server root.
func (Module) RootRoutes() []api.Route {
	return []api.Route{{Method: http.MethodGet, Pattern: "/.well-known/terraform.json"}}
}

// ServeRoot implements api.RootRouter. The discovery document can point at
// only one feed per host: the lexicographically first terraform feed wins
// (run one terraform feed per host, or front feeds with distinct hostnames).
func (Module) ServeRoot(w http.ResponseWriter, _ *http.Request, feeds []api.Feed) {
	if len(feeds) == 0 {
		http.Error(w, "no terraform feeds configured", http.StatusNotFound)
		return
	}
	// With a group configured it is the group clients are meant to reach:
	// its members are there to be combined, not addressed.
	target := names(feeds)[0]
	for _, f := range feeds {
		if f.Group {
			target = f.Name
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"modules.v1": "/terraform/" + target + "/v1/modules/",
	})
}

// RedirectSafeIntent implements api.RedirectSafe: module archives may be
// answered with a pre-signed redirect; go-getter follows it.
func (Module) RedirectSafeIntent(intent api.Intent) bool {
	return intent.Kind == api.IntentArtifact
}
