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
	"sort"
	"strings"
	"time"

	"github.com/sasokolov/package-registry/core/api"
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
		downloadPath := strings.TrimSuffix(p, archiveFile) + "download"
		return api.Intent{
			Kind:        api.IntentArtifact,
			Coord:       coord(rest, rest[3]),
			RemotePath:  downloadPath,
			Indirect:    true,
			ContentType: "application/gzip",
		}, nil
	default:
		return api.Intent{}, api.NotFoundf("unsupported module registry path: %q", p)
	}
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
// 204 + X-Terraform-Get pointing at the registry's own archive path,
// resolved by the client relative to the download URL.
func (Module) Synthesize(_ api.Feed, intent api.Intent) (api.SyntheticResponse, error) {
	if intent.Kind != api.IntentSynthetic || !strings.HasSuffix(intent.RemotePath, "/download") {
		return api.SyntheticResponse{}, api.NotFoundf("unexpected synthetic intent %q", intent.RemotePath)
	}
	return api.SyntheticResponse{
		Status: http.StatusNoContent,
		Header: map[string]string{"X-Terraform-Get": archiveFile},
	}, nil
}

// ResolveIndirect implements api.IndirectResolver: extract the module
// archive location from the upstream download response.
func (Module) ResolveIndirect(_ api.Feed, _ api.Intent, status int, header map[string][]string, _ []byte) (string, error) {
	if status != http.StatusNoContent && status != http.StatusOK {
		return "", fmt.Errorf("upstream download endpoint returned status %d", status)
	}
	vals := header[http.CanonicalHeaderKey("X-Terraform-Get")]
	if len(vals) == 0 || vals[0] == "" {
		return "", errors.New("upstream download response lacks X-Terraform-Get")
	}
	return vals[0], nil
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
	names := make([]string, 0, len(feeds))
	for _, f := range feeds {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"modules.v1": "/terraform/" + names[0] + "/v1/modules/",
	})
}
