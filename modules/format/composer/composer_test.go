package composer

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sasokolov/package-registry/core/api"
)

func parse(t *testing.T, path string) (api.Intent, error) {
	t.Helper()
	return Module{}.Parse(httptest.NewRequest("GET", path, nil))
}

func TestParse(t *testing.T) {
	intent, err := parse(t, "/packages.json")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentMetadata || intent.RemotePath != "packages.json" {
		t.Errorf("root intent = %+v", intent)
	}

	intent, err = parse(t, "/p2/vendor/pkg.json")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Coord.Name != "vendor/pkg" || intent.CacheTTL != metadataTTL {
		t.Errorf("package intent = %+v", intent)
	}

	// ~dev documents describe the same package.
	intent, err = parse(t, "/p2/vendor/pkg~dev.json")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Coord.Name != "vendor/pkg" {
		t.Errorf("dev coord = %+v", intent.Coord)
	}
	if intent.RemotePath != "p2/vendor/pkg~dev.json" {
		t.Errorf("dev remote path = %q", intent.RemotePath)
	}

	// Dist locations are absolute upstream URLs encoded into the path.
	distURL := "https://api.github.com/repos/vendor/pkg/zipball/abc123"
	encoded := base64.RawURLEncoding.EncodeToString([]byte(distURL))
	intent, err = parse(t, "/dists/"+encoded+"/pkg-abc123.zip")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentArtifact {
		t.Errorf("dist intent kind = %s", intent.Kind)
	}
	if intent.RemoteURL != distURL {
		t.Errorf("dist remote url = %q, want %q", intent.RemoteURL, distURL)
	}
	if intent.Coord.Name != "pkg" {
		t.Errorf("dist coord = %+v", intent.Coord)
	}
}

func TestParseRejectsBadDistLocation(t *testing.T) {
	// Only absolute http(s) URLs may be reached; anything else is a 404,
	// never an attempt to fetch it.
	for _, loc := range []string{"file:///etc/passwd", "/relative/path", "ftp://h/x.zip"} {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(loc))
		if _, err := parse(t, "/dists/"+encoded+"/x.zip"); !errors.Is(err, api.ErrNotFound) {
			t.Errorf("dist location %q accepted", loc)
		}
	}
	if _, err := parse(t, "/dists/not-base64!/x.zip"); !errors.Is(err, api.ErrNotFound) {
		t.Error("malformed dist encoding accepted")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, p := range []string{"/", "/p2/nopkg.json", "/p2/../x.json", "/unknown", "/dists/", "/dists/x"} {
		if _, err := parse(t, p); !errors.Is(err, api.ErrNotFound) {
			t.Errorf("Parse(%q) = %v, want ErrNotFound", p, err)
		}
	}
}

func TestRewriteMetadataGolden(t *testing.T) {
	feed := api.Feed{Name: "packagist", Format: "composer", ExternalURL: "https://registry.local"}
	for _, name := range []string{"packages", "package"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := Module{}.RewriteMetadata(feed, raw)
			if err != nil {
				t.Fatalf("RewriteMetadata: %v", err)
			}
			golden, err := os.ReadFile(filepath.Join("testdata", name+".golden.json"))
			if err != nil {
				t.Fatal(err)
			}
			var a, b any
			if err := json.Unmarshal(got, &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(golden, &b); err != nil {
				t.Fatal(err)
			}
			ab, _ := json.Marshal(a)
			bb, _ := json.Marshal(b)
			if string(ab) != string(bb) {
				t.Errorf("output differs from golden:\n%s", got)
			}
		})
	}
}

func TestRewriteRootManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "packages.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Module{}.RewriteMetadata(
		api.Feed{Name: "packagist", Format: "composer", ExternalURL: "https://registry.local"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["metadata-url"] != "https://registry.local/composer/packagist/p2/%package%.json" {
		t.Errorf("metadata-url = %v", doc["metadata-url"])
	}
	for _, gone := range []string{"providers-url", "provider-includes", "list"} {
		if _, ok := doc[gone]; ok {
			t.Errorf("root manifest still advertises %q, which this registry does not serve", gone)
		}
	}
	// Search IS served — proxied with the query, or answered locally by a
	// hosting feed — so it is pointed here rather than removed. Left as the
	// upstream wrote it, the client would search past the registry.
	if search, ok := doc["search"]; ok {
		if search != "https://registry.local/composer/packagist/search.json?q=%query%&type=%type%" {
			t.Errorf("search = %v, want it pointed at the registry", search)
		}
	}
}

func TestRewriteDistURLs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Module{}.RewriteMetadata(
		api.Feed{Name: "packagist", Format: "composer", ExternalURL: "https://registry.local"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	if !strings.Contains(body, "https://registry.local/composer/packagist/dists/") {
		t.Errorf("dist urls not rewritten:\n%s", body)
	}
	if strings.Contains(body, "api.github.com") {
		t.Errorf("upstream dist url still present:\n%s", body)
	}
	// Shasums must survive so clients can verify.
	if !strings.Contains(body, `"shasum"`) {
		t.Error("dist.shasum was dropped")
	}

	// The rewritten dist URL must parse back into a dist intent.
	var doc map[string]any
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	packages := doc["packages"].(map[string]any)
	versions := packages["vendor/pkg"].([]any)
	dist := versions[0].(map[string]any)["dist"].(map[string]any)
	rewritten := dist["url"].(string)
	requestPath := strings.TrimPrefix(rewritten, "https://registry.local/composer/packagist")
	back, err := parse(t, requestPath)
	if err != nil {
		t.Fatalf("rewritten dist url %q does not parse back: %v", requestPath, err)
	}
	if back.RemoteURL != "https://api.github.com/repos/vendor/pkg/zipball/abc123" {
		t.Errorf("round-tripped dist location = %q", back.RemoteURL)
	}
}

func TestExtractMetadata(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := Module{}.ExtractMetadata(
		api.PackageCoordinate{Format: "composer", Name: "vendor/pkg", Version: "1.0.0"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if meta[api.MetaEcosystem] != "Packagist" {
		t.Errorf("ecosystem = %q", meta[api.MetaEcosystem])
	}
	if meta[api.MetaLicense] != "MIT" {
		t.Errorf("license = %q", meta[api.MetaLicense])
	}
	if meta[api.MetaPublishedAt] == "" {
		t.Error("published_at missing")
	}
}
