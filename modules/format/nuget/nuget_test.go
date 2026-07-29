package nuget

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sasokolov/package-registry/core/api"
)

func parse(t *testing.T, path string) (api.Intent, error) {
	t.Helper()
	return Module{}.Parse(httptest.NewRequest("GET", path, nil))
}

func TestParse(t *testing.T) {
	intent, err := parse(t, "/v3/index.json")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentSynthetic {
		t.Errorf("service index kind = %s, want synthetic", intent.Kind)
	}

	// Flat container paths are mapped onto the upstream layout.
	intent, err = parse(t, "/v3/flat2/newtonsoft.json/index.json")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentMetadata || intent.Coord.Name != "newtonsoft.json" {
		t.Errorf("version list intent = %+v", intent)
	}
	if intent.RemotePath != "v3-flatcontainer/newtonsoft.json/index.json" {
		t.Errorf("remote path = %q", intent.RemotePath)
	}

	intent, err = parse(t, "/v3/flat2/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentArtifact {
		t.Fatalf("nupkg kind = %s", intent.Kind)
	}
	if intent.Coord.Version != "13.0.3" || intent.Coord.Name != "newtonsoft.json" {
		t.Errorf("nupkg coord = %+v", intent.Coord)
	}
	if intent.RemotePath != "v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg" {
		t.Errorf("nupkg remote path = %q", intent.RemotePath)
	}

	intent, err = parse(t, "/v3/registration/newtonsoft.json/index.json")
	if err != nil {
		t.Fatal(err)
	}
	if intent.RemotePath != "v3/registration5-gz-semver2/newtonsoft.json/index.json" {
		t.Errorf("registration remote path = %q", intent.RemotePath)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, p := range []string{"/", "/v3", "/v4/index.json", "/v3/flat2/", "/v3/flat2/a/b/c/d", "/v3/registration/"} {
		if _, err := parse(t, p); !errors.Is(err, api.ErrNotFound) {
			t.Errorf("Parse(%q) = %v, want ErrNotFound", p, err)
		}
	}
}

func TestSynthesizeServiceIndex(t *testing.T) {
	intent, err := parse(t, "/v3/index.json")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := Module{}.Synthesize(
		api.Feed{Name: "nugetorg", Format: "nuget", ExternalURL: "https://registry.local"}, intent)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d", resp.Status)
	}
	var doc serviceIndex
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "3.0.0" || len(doc.Resources) == 0 {
		t.Fatalf("service index = %+v", doc)
	}
	var haveFlat, haveRegistration bool
	for _, r := range doc.Resources {
		if !strings.HasPrefix(r.ID, "https://registry.local/nuget/nugetorg/") {
			t.Errorf("resource %s points outside the registry: %s", r.Type, r.ID)
		}
		switch r.Type {
		case "PackageBaseAddress/3.0.0":
			haveFlat = true
		case "RegistrationsBaseUrl/3.6.0":
			haveRegistration = true
		}
	}
	if !haveFlat || !haveRegistration {
		t.Error("service index misses the resources dotnet restore needs")
	}
}

func TestRewriteMetadataRepointsEndpoints(t *testing.T) {
	doc := map[string]any{
		"@id":   "https://api.nuget.org/v3/registration5-gz-semver2/newtonsoft.json/index.json",
		"count": 1,
		"items": []any{map[string]any{
			"@id":            "https://api.nuget.org/v3/registration5-gz-semver2/newtonsoft.json/page/1.0.0/13.0.3.json",
			"packageContent": "https://api.nuget.org/v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg",
			"catalogEntry": map[string]any{
				"licenseUrl":     "https://licenses.nuget.org/MIT",
				"projectUrl":     "https://www.newtonsoft.com/json",
				"packageContent": "https://api.nuget.org/v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg",
			},
		}},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	feed := api.Feed{Name: "nugetorg", Format: "nuget", ExternalURL: "https://registry.local"}
	got, err := Module{}.RewriteMetadata(feed, raw)
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	if strings.Contains(body, "api.nuget.org") {
		t.Errorf("upstream URLs survived the rewrite:\n%s", body)
	}
	if !strings.Contains(body, "https://registry.local/nuget/nugetorg/v3/flat2/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg") {
		t.Errorf("packageContent not repointed:\n%s", body)
	}
	if !strings.Contains(body, "https://registry.local/nuget/nugetorg/v3/registration/newtonsoft.json/index.json") {
		t.Errorf("registration @id not repointed:\n%s", body)
	}
	// Unrelated URLs must survive untouched.
	if !strings.Contains(body, "https://licenses.nuget.org/MIT") ||
		!strings.Contains(body, "https://www.newtonsoft.com/json") {
		t.Errorf("unrelated URLs were rewritten:\n%s", body)
	}

	// The rewritten packageContent must parse back into an artifact intent.
	var out map[string]any
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	items := out["items"].([]any)
	content := items[0].(map[string]any)["packageContent"].(string)
	path := strings.TrimPrefix(content, "https://registry.local/nuget/nugetorg")
	back, err := parse(t, path)
	if err != nil || back.Kind != api.IntentArtifact {
		t.Errorf("rewritten packageContent does not parse back: %v (%+v)", err, back)
	}
}

func TestRewriteMetadataDecompressesGzip(t *testing.T) {
	plain := []byte(`{"@id":"https://api.nuget.org/v3-flatcontainer/pkg/1.0.0/pkg.1.0.0.nupkg"}`)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := Module{}.RewriteMetadata(
		api.Feed{Name: "nugetorg", Format: "nuget", ExternalURL: "https://registry.local"}, buf.Bytes())
	if err != nil {
		t.Fatalf("gzipped registration rejected: %v", err)
	}
	if !strings.Contains(string(got), "registry.local/nuget/nugetorg/v3/flat2/pkg/1.0.0/pkg.1.0.0.nupkg") {
		t.Errorf("gzipped document not rewritten: %s", got)
	}
}

// The address in everybody's head is the service index, because that is what
// goes in a nuget.config. Joining protocol paths onto it produces
// ".../v3/index.json/v3-flatcontainer/..." and a 400 that explains nothing —
// so it is refused at load with the address to use instead.
func TestAServiceIndexUpstreamIsRefused(t *testing.T) {
	var module Module
	err := module.ValidateFeeds([]api.Feed{
		{Name: "nugetorg", Upstream: "https://api.nuget.org/v3/index.json"},
	})
	if err == nil {
		t.Fatal("a service-index upstream was accepted")
	}
	if !strings.Contains(err.Error(), "https://api.nuget.org") {
		t.Errorf("the error does not say what to write instead: %v", err)
	}
	if !strings.Contains(err.Error(), "service index") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

// The host root is what the protocol needs, and a hosted feed has no
// upstream at all: neither may be refused.
func TestOrdinaryNuGetUpstreamsAreAccepted(t *testing.T) {
	cases := [][]api.Feed{
		{{Name: "nugetorg", Upstream: "https://api.nuget.org"}},
		{{Name: "nugetorg", Upstream: "https://api.nuget.org/"}},
		// An upstream served under a sub-path, which the rewriter already
		// supports.
		{{Name: "mirror", Upstream: "http://mirror.example/nuget"}},
		{{Name: "hosted", Hosted: true}},
	}
	var module Module
	for _, feeds := range cases {
		if err := module.ValidateFeeds(feeds); err != nil {
			t.Errorf("ValidateFeeds(%+v) = %v, want accepted", feeds, err)
		}
	}
}

// A registry has to be able to answer what it asks an upstream for, or it
// cannot be part of a chain — and a remote site caching through the site
// that holds the upstream link is exactly the topology geo replication does
// not cover, because a cache is site-local by design.
//
// Both spellings must also produce the same intent, or the two would occupy
// separate cache entries for one package.
func TestTheUpstreamLayoutIsAnsweredAsWellAsAsked(t *testing.T) {
	tests := []struct {
		client   string
		upstream string
	}{
		{"v3/flat2/newtonsoft.json/index.json", "v3-flatcontainer/newtonsoft.json/index.json"},
		{"v3/flat2/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg",
			"v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg"},
		{"v3/registration/newtonsoft.json/index.json",
			"v3/registration5-gz-semver2/newtonsoft.json/index.json"},
	}
	for _, tc := range tests {
		asClient, err := parse(t, "/"+tc.client)
		if err != nil {
			t.Fatalf("client path %q: %v", tc.client, err)
		}
		asUpstream, err := parse(t, "/"+tc.upstream)
		if err != nil {
			t.Fatalf("upstream path %q: %v", tc.upstream, err)
		}
		if asClient != asUpstream {
			t.Errorf("%q and %q resolve differently:\n  %+v\n  %+v",
				tc.client, tc.upstream, asClient, asUpstream)
		}
	}
}

// The aliases must not accept nonsense that the client paths reject.
func TestUpstreamShapedNonsenseIsStillRefused(t *testing.T) {
	for _, p := range []string{
		"/v3-flatcontainer/",
		"/v3-flatcontainer/newtonsoft.json",
		"/v3-flatcontainer/a/b/c/d",
		"/v3/registration5-gz-semver2/",
	} {
		if _, err := parse(t, p); err == nil {
			t.Errorf("Parse(%q) was accepted", p)
		}
	}
}
