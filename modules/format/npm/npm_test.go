package npm

import (
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

func TestParsePackageRoot(t *testing.T) {
	tests := []struct {
		path string
		name string
	}{
		{"/lodash", "lodash"},
		{"/@scope%2fpkg", "@scope/pkg"},
		{"/@scope/pkg", "@scope/pkg"}, // some clients do not escape
	}
	for _, tt := range tests {
		intent, err := parse(t, tt.path)
		if err != nil {
			t.Fatalf("%s: %v", tt.path, err)
		}
		if intent.Kind != api.IntentMetadata {
			t.Errorf("%s: kind = %s", tt.path, intent.Kind)
		}
		if intent.Coord.Name != tt.name {
			t.Errorf("%s: name = %q, want %q", tt.path, intent.Coord.Name, tt.name)
		}
		if intent.CacheTTL != metadataTTL {
			t.Errorf("%s: ttl = %s", tt.path, intent.CacheTTL)
		}
	}
}

func TestParseTarball(t *testing.T) {
	intent, err := parse(t, "/lodash/-/lodash-4.17.21.tgz")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentArtifact {
		t.Fatalf("kind = %s", intent.Kind)
	}
	if intent.Coord.Name != "lodash" || intent.Coord.Version != "4.17.21" {
		t.Errorf("coord = %+v", intent.Coord)
	}
	if intent.RemotePath != "lodash/-/lodash-4.17.21.tgz" {
		t.Errorf("remote path = %q", intent.RemotePath)
	}

	// Scoped tarballs drop the scope from the filename.
	intent, err = parse(t, "/@scope/pkg/-/pkg-1.2.3.tgz")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Coord.Name != "@scope/pkg" || intent.Coord.Version != "1.2.3" {
		t.Errorf("scoped coord = %+v", intent.Coord)
	}
}

func TestParseDistTags(t *testing.T) {
	intent, err := parse(t, "/-/package/lodash/dist-tags")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentMetadata || intent.Coord.Name != "lodash" {
		t.Errorf("intent = %+v", intent)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, p := range []string{"/", "/../etc/passwd", "/@scope", "/a/b/c", "/-/package/x"} {
		if _, err := parse(t, p); !errors.Is(err, api.ErrNotFound) {
			t.Errorf("Parse(%q) = %v, want ErrNotFound", p, err)
		}
	}
}

// TestRewriteMetadataGolden covers six real-world package-root shapes:
// modern, scoped, ancient (no integrity, license object), dual-licensed
// (license array), a private host with port and deep path, and a messy one
// (null fields, unicode, deprecated versions).
func TestRewriteMetadataGolden(t *testing.T) {
	feed := api.Feed{Name: "npmjs", Format: "npm", ExternalURL: "https://registry.local"}
	fixtures, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, path := range fixtures {
		if strings.HasSuffix(path, ".golden.json") {
			continue
		}
		checked++
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Module{}.RewriteMetadata(feed, raw)
			if err != nil {
				t.Fatalf("RewriteMetadata: %v", err)
			}

			goldenPath := strings.TrimSuffix(path, ".json") + ".golden.json"
			golden, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(t, got, golden) {
				t.Errorf("output differs from golden %s:\n%s", goldenPath, got)
			}

			// Every tarball must now point at this registry, and the
			// integrity/shasum fields must survive untouched.
			var out, in map[string]any
			if err := json.Unmarshal(got, &out); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				t.Fatal(err)
			}
			outVersions, _ := out["versions"].(map[string]any)
			inVersions, _ := in["versions"].(map[string]any)
			for version, rawVersion := range outVersions {
				dist, _ := rawVersion.(map[string]any)["dist"].(map[string]any)
				if dist == nil {
					continue
				}
				tarball, _ := dist["tarball"].(string)
				if !strings.HasPrefix(tarball, "https://registry.local/npm/npmjs/") {
					t.Errorf("version %s: tarball not rewritten: %q", version, tarball)
				}
				inDist, _ := inVersions[version].(map[string]any)["dist"].(map[string]any)
				for _, key := range []string{"integrity", "shasum", "signatures", "fileCount"} {
					if !equalJSON(inDist[key], dist[key]) {
						t.Errorf("version %s: dist.%s changed", version, key)
					}
				}
			}
		})
	}
	if checked < 6 {
		t.Fatalf("only %d package-root fixtures; PLAN requires at least 6", checked)
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return equalJSON(x, y)
}

func equalJSON(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

func TestRewriteMetadataRelativeBase(t *testing.T) {
	// Without site.external_url the rewritten URL is root-relative, which
	// npm resolves against the registry it was pointed at.
	raw, err := os.ReadFile(filepath.Join("testdata", "modern.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Module{}.RewriteMetadata(api.Feed{Name: "npmjs", Format: "npm"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"/npm/npmjs/modern-pkg/-/modern-pkg-1.0.0.tgz"`) {
		t.Errorf("relative rewrite missing:\n%s", got)
	}
}

func TestExtractMetadata(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "modern.json"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := Module{}.ExtractMetadata(
		api.PackageCoordinate{Format: "npm", Name: "modern-pkg", Version: "2.0.0"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if meta[api.MetaEcosystem] != "npm" {
		t.Errorf("ecosystem = %q", meta[api.MetaEcosystem])
	}
	if meta[api.MetaLicense] != "MIT" {
		t.Errorf("license = %q", meta[api.MetaLicense])
	}
	if meta[api.MetaPublishedAt] != "2021-06-01T12:00:00.000Z" {
		t.Errorf("published_at = %q", meta[api.MetaPublishedAt])
	}
	if !strings.HasPrefix(meta[api.MetaChecksum], "sha512:") {
		t.Errorf("checksum = %q, want sha512 from dist.integrity", meta[api.MetaChecksum])
	}

	// Legacy package: only shasum, license as an object.
	raw, err = os.ReadFile(filepath.Join("testdata", "ancient.json"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err = Module{}.ExtractMetadata(
		api.PackageCoordinate{Format: "npm", Name: "ancient", Version: "0.0.3"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if meta[api.MetaLicense] != "BSD" {
		t.Errorf("license from object = %q", meta[api.MetaLicense])
	}
	if !strings.HasPrefix(meta[api.MetaChecksum], "sha1:") {
		t.Errorf("checksum = %q, want sha1 fallback", meta[api.MetaChecksum])
	}

	// License array of objects.
	raw, err = os.ReadFile(filepath.Join("testdata", "dual-license.json"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err = Module{}.ExtractMetadata(
		api.PackageCoordinate{Format: "npm", Name: "dual-licensed", Version: "1.0.0"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if meta[api.MetaLicense] != "MIT OR GPL-2.0" {
		t.Errorf("license array = %q", meta[api.MetaLicense])
	}
}

func TestMetadataIntent(t *testing.T) {
	m := Module{}
	intent, ok := m.MetadataIntent(api.Feed{}, api.PackageCoordinate{Name: "@scope/pkg", Version: "1.0.0"})
	if !ok {
		t.Fatal("no metadata intent")
	}
	if intent.RemotePath != "@scope/pkg" {
		t.Errorf("remote path = %q", intent.RemotePath)
	}
	if _, ok := m.MetadataIntent(api.Feed{}, api.PackageCoordinate{Name: "lodash"}); ok {
		t.Error("version-less coordinate got a metadata intent")
	}
}
