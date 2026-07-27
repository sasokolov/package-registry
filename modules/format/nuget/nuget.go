// Package nuget implements the FormatModule for the NuGet V3 protocol,
// enough for `dotnet restore`:
//
//	GET /v3/index.json                       service index (synthesized)
//	GET /v3/registration/{id}/index.json     registration index (mutable)
//	GET /v3/flat2/{id}/index.json            version list (mutable)
//	GET /v3/flat2/{id}/{ver}/{file}.nupkg    package (immutable artifact)
//	GET /v3/query?...                        search passthrough (mutable)
//
// The service index is answered locally so every resource URL points at this
// registry; everything else goes through the generic pipeline. Registry
// paths are mapped onto the nuget.org upstream layout
// (v3-flatcontainer/, v3/registration5-gz-semver2/), and gzipped
// registration documents are decompressed before rewriting so clients get
// plain JSON with registry URLs.
package nuget

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

// metadataTTL bounds registration/version documents (SWR beyond it).
const metadataTTL = 5 * time.Minute

// Upstream layout (nuget.org and compatible mirrors). Registry paths are
// stable and vendor-neutral; these prefixes map them onto the upstream.
const (
	upstreamFlatPrefix         = "v3-flatcontainer/"
	upstreamRegistrationPrefix = "v3/registration5-gz-semver2/"
)

func init() {
	api.RegisterFormat(Module{})
}

// Module implements api.FormatModule.
type Module struct{}

// Name implements api.FormatModule.
func (Module) Name() string { return "nuget" }

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
	if p == "" || strings.Contains(p, "..") {
		return api.Intent{}, api.NotFoundf("invalid path %q", p)
	}

	switch {
	case p == "v3/index.json":
		return api.Intent{
			Kind:        api.IntentSynthetic,
			Coord:       api.PackageCoordinate{Format: "nuget", Name: "service-index"},
			RemotePath:  p,
			ContentType: "application/json",
		}, nil

	case strings.HasPrefix(p, "v3/registration/"):
		rest := strings.TrimPrefix(p, "v3/registration/")
		id, _, ok := strings.Cut(rest, "/")
		if !ok || id == "" {
			return api.Intent{}, api.NotFoundf("not a registration path: %q", p)
		}
		return api.Intent{
			Kind:        api.IntentMetadata,
			Coord:       api.PackageCoordinate{Format: "nuget", Name: id},
			CacheTTL:    metadataTTL,
			RemotePath:  upstreamRegistrationPrefix + rest,
			ContentType: "application/json",
		}, nil

	case strings.HasPrefix(p, "v3/flat2/"):
		return parseFlatContainer(p)

	case strings.HasPrefix(p, "v3/query"):
		return api.Intent{
			Kind:        api.IntentMetadata,
			Coord:       api.PackageCoordinate{Format: "nuget", Name: "search"},
			CacheTTL:    time.Minute,
			RemotePath:  p,
			ContentType: "application/json",
		}, nil

	default:
		return api.Intent{}, api.NotFoundf("unsupported nuget path: %q", p)
	}
}

// parseFlatContainer handles the package base address (flat container):
// {id}/index.json lists versions, {id}/{version}/{file} serves content.
func parseFlatContainer(p string) (api.Intent, error) {
	rest := strings.TrimPrefix(p, "v3/flat2/")
	segs := strings.Split(rest, "/")
	for _, s := range segs {
		if s == "" {
			return api.Intent{}, api.NotFoundf("invalid flat container path: %q", p)
		}
	}
	switch {
	case len(segs) == 2 && segs[1] == "index.json":
		return api.Intent{
			Kind:        api.IntentMetadata,
			Coord:       api.PackageCoordinate{Format: "nuget", Name: segs[0]},
			CacheTTL:    metadataTTL,
			RemotePath:  upstreamFlatPrefix + rest,
			ContentType: "application/json",
		}, nil
	case len(segs) == 3:
		contentType := "application/octet-stream"
		if strings.HasSuffix(segs[2], ".nuspec") {
			contentType = "application/xml"
		}
		return api.Intent{
			Kind: api.IntentArtifact,
			Coord: api.PackageCoordinate{
				Format: "nuget", Name: segs[0], Version: segs[1],
			},
			RemotePath:  upstreamFlatPrefix + rest,
			ContentType: contentType,
		}, nil
	default:
		return api.Intent{}, api.NotFoundf("unsupported flat container path: %q", p)
	}
}

// serviceIndex is the NuGet V3 service index.
type serviceIndex struct {
	Version   string            `json:"version"`
	Resources []serviceResource `json:"resources"`
}

type serviceResource struct {
	ID      string `json:"@id"`
	Type    string `json:"@type"`
	Comment string `json:"comment,omitempty"`
}

// Synthesize implements api.Synthesizer: the service index is generated so
// every advertised resource points at this registry, never at the upstream.
func (Module) Synthesize(feed api.Feed, intent api.Intent) (api.SyntheticResponse, error) {
	if intent.Kind != api.IntentSynthetic {
		return api.SyntheticResponse{}, api.NotFoundf("unexpected synthetic intent %q", intent.RemotePath)
	}
	base := feedBase(feed)
	doc := serviceIndex{
		Version: "3.0.0",
		Resources: []serviceResource{
			{ID: base + "/v3/flat2/", Type: "PackageBaseAddress/3.0.0",
				Comment: "Flat container served by this registry"},
			{ID: base + "/v3/registration/", Type: "RegistrationsBaseUrl/3.6.0"},
			{ID: base + "/v3/registration/", Type: "RegistrationsBaseUrl/3.4.0"},
			{ID: base + "/v3/registration/", Type: "RegistrationsBaseUrl"},
			{ID: base + "/v3/query", Type: "SearchQueryService/3.5.0"},
			{ID: base + "/v3/query", Type: "SearchQueryService"},
		},
	}
	if feed.Hosted {
		// Only a feed that accepts writes advertises where to send them.
		// Announcing the endpoint everywhere would send `dotnet nuget push`
		// at a proxy and leave it to interpret a 405.
		doc.Resources = append(doc.Resources, serviceResource{
			ID: base + publishPath, Type: "PackagePublish/2.0.0",
			Comment: "Push endpoint of this hosted feed",
		})
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return api.SyntheticResponse{}, fmt.Errorf("encode service index: %w", err)
	}
	return api.SyntheticResponse{
		Status: http.StatusOK,
		Header: map[string]string{"Content-Type": "application/json"},
		Body:   append(body, '\n'),
	}, nil
}

func feedBase(feed api.Feed) string {
	prefix := "/nuget/" + feed.Name
	if feed.ExternalURL != "" {
		return strings.TrimSuffix(feed.ExternalURL, "/") + prefix
	}
	return prefix
}

// RewriteMetadata implements api.FormatModule: registration and search
// documents embed absolute URLs pointing at the upstream; they are
// rewritten so clients keep talking to this registry.
func (Module) RewriteMetadata(feed api.Feed, body []byte) ([]byte, error) {
	// nuget.org serves registration documents gzipped; decompress so the
	// client receives plain JSON carrying registry URLs.
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("decompress nuget metadata: %w", err)
		}
		defer func() { _ = zr.Close() }()
		plain, err := io.ReadAll(io.LimitReader(zr, 128<<20))
		if err != nil {
			return nil, fmt.Errorf("decompress nuget metadata: %w", err)
		}
		body = plain
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		// Not JSON (or an unexpected shape): pass through untouched.
		return body, nil
	}
	base := feedBase(feed)
	rewritten := rewriteURLs(doc, base)
	out, err := json.Marshal(rewritten)
	if err != nil {
		return nil, fmt.Errorf("encode nuget metadata: %w", err)
	}
	return out, nil
}

// rewriteURLs walks the document and repoints known NuGet V3 endpoints at
// this registry. Only the well-known path shapes are rewritten, so unrelated
// URLs (project sites, license URLs) are preserved.
func rewriteURLs(node any, base string) any {
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			if s, ok := child.(string); ok {
				if replaced, changed := rewriteEndpoint(s, base); changed {
					v[key] = replaced
					continue
				}
			}
			v[key] = rewriteURLs(child, base)
		}
		return v
	case []any:
		for i, child := range v {
			v[i] = rewriteURLs(child, base)
		}
		return v
	default:
		return node
	}
}

// knownEndpoints maps upstream path markers onto this registry's paths.
var knownEndpoints = []struct{ marker, local string }{
	{"/v3/registration5-gz-semver2/", "/v3/registration/"},
	{"/v3/registration5-semver1/", "/v3/registration/"},
	{"/v3/registration3-gz-semver2/", "/v3/registration/"},
	{"/v3/registration/", "/v3/registration/"},
	{"/v3-flatcontainer/", "/v3/flat2/"},
	{"/v3/flat2/", "/v3/flat2/"},
}

func rewriteEndpoint(raw, base string) (string, bool) {
	// Markers are matched anywhere in the path, so an upstream served under
	// a sub-path (http://host/nuget-upstream/v3-flatcontainer/...) maps
	// correctly without extra configuration.
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return raw, false
	}
	for _, e := range knownEndpoints {
		if i := strings.Index(raw, e.marker); i >= 0 {
			return base + e.local + raw[i+len(e.marker):], true
		}
	}
	return raw, false
}

// RedirectSafeIntent implements api.RedirectSafe: flat-container packages
// may be answered with a pre-signed redirect; registration documents carry
// rewritten URLs and must be streamed.
func (Module) RedirectSafeIntent(intent api.Intent) bool {
	return intent.Kind == api.IntentArtifact
}
