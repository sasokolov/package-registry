// Package echomodule is a test-only FormatModule for conformance runs: it
// proxies arbitrary files from the feed's upstream 1:1. Paths under meta/
// are treated as mutable metadata (short TTL, stale-while-revalidate),
// everything else as immutable artifacts. It is linked into the registry
// binary only with the "conformance" build tag.
package echomodule

import (
	"net/http"
	"strings"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

// MetadataTTL is deliberately short so conformance scenarios can observe
// stale serving without long sleeps.
const MetadataTTL = 5 * time.Second

func init() {
	api.RegisterFormat(Module{})
}

// Module implements api.FormatModule.
type Module struct{}

// Name implements api.FormatModule.
func (Module) Name() string { return "echo" }

// Routes implements api.FormatModule.
func (Module) Routes() []api.Route {
	return []api.Route{{Method: http.MethodGet, Pattern: "/*"}}
}

// Parse implements api.FormatModule.
func (Module) Parse(r *http.Request) (api.Intent, error) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		return api.Intent{}, api.NotFoundf("empty path")
	}
	intent := api.Intent{
		Kind:       api.IntentArtifact,
		Coord:      api.PackageCoordinate{Format: "echo", Name: p},
		RemotePath: p,
	}
	if strings.HasPrefix(p, "meta/") {
		intent.Kind = api.IntentMetadata
		intent.CacheTTL = MetadataTTL
	}
	return intent, nil
}

// RewriteMetadata implements api.FormatModule (identity: nothing to rewrite).
func (Module) RewriteMetadata(_ api.Feed, body []byte) ([]byte, error) {
	return body, nil
}
