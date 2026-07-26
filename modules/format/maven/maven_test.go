package maven

import (
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

func TestParseArtifact(t *testing.T) {
	intent, err := parse(t, "/org/slf4j/slf4j-api/2.0.13/slf4j-api-2.0.13.jar")
	if err != nil {
		t.Fatal(err)
	}
	want := api.Intent{
		Kind:           api.IntentArtifact,
		Coord:          api.PackageCoordinate{Format: "maven", Name: "org.slf4j:slf4j-api", Version: "2.0.13"},
		RemotePath:     "org/slf4j/slf4j-api/2.0.13/slf4j-api-2.0.13.jar",
		RemoteChecksum: api.ChecksumSource{Algo: "sha1", Path: "org/slf4j/slf4j-api/2.0.13/slf4j-api-2.0.13.jar.sha1"},
		ContentType:    "application/java-archive",
	}
	if intent != want {
		t.Errorf("intent = %+v\nwant   = %+v", intent, want)
	}
}

func TestParseChecksumSidecar(t *testing.T) {
	for ext, algo := range map[string]string{".sha1": "sha1", ".md5": "md5", ".sha256": "sha256", ".sha512": "sha512"} {
		intent, err := parse(t, "/com/example/liba/1.0.0/liba-1.0.0.jar"+ext)
		if err != nil {
			t.Fatalf("%s: %v", ext, err)
		}
		if intent.WantChecksum != algo {
			t.Errorf("%s: WantChecksum = %q, want %q", ext, intent.WantChecksum, algo)
		}
		if intent.RemotePath != "com/example/liba/1.0.0/liba-1.0.0.jar" {
			t.Errorf("%s: RemotePath = %q (checksum suffix must be stripped)", ext, intent.RemotePath)
		}
		if intent.Kind != api.IntentArtifact {
			t.Errorf("%s: kind = %s", ext, intent.Kind)
		}
		if intent.ContentType != "text/plain" {
			t.Errorf("%s: content type = %q", ext, intent.ContentType)
		}
		if intent.Coord.Version != "1.0.0" || intent.Coord.Name != "com.example:liba" {
			t.Errorf("%s: coord = %+v", ext, intent.Coord)
		}
	}
}

func TestParsePomContentType(t *testing.T) {
	intent, err := parse(t, "/com/example/liba/1.0.0/liba-1.0.0.pom")
	if err != nil {
		t.Fatal(err)
	}
	if intent.ContentType != "application/xml" {
		t.Errorf("pom content type = %q", intent.ContentType)
	}
}

func TestParseMetadata(t *testing.T) {
	intent, err := parse(t, "/com/example/liba/maven-metadata.xml")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentMetadata {
		t.Fatalf("kind = %s", intent.Kind)
	}
	if intent.CacheTTL != metadataTTL {
		t.Errorf("ttl = %s", intent.CacheTTL)
	}
	if intent.Coord.Name != "com.example:liba" || intent.Coord.Version != "" {
		t.Errorf("coord = %+v", intent.Coord)
	}
	if intent.RemotePath != "com/example/liba/maven-metadata.xml" {
		t.Errorf("remote path = %q", intent.RemotePath)
	}

	// Sidecar checksum of metadata.
	intent, err = parse(t, "/com/example/liba/maven-metadata.xml.sha1")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != api.IntentMetadata || intent.WantChecksum != "sha1" {
		t.Errorf("metadata checksum intent = %+v", intent)
	}

	// Group-level metadata (e.g. maven plugin groups).
	intent, err = parse(t, "/plugins/maven-metadata.xml")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Coord.Name != "plugins" {
		t.Errorf("group-level coord = %+v", intent.Coord)
	}
}

func TestParseSnapshotRejectedWithClearMessage(t *testing.T) {
	for _, p := range []string{
		"/com/example/liba/1.0.0-SNAPSHOT/liba-1.0.0-SNAPSHOT.jar",
		"/com/example/liba/1.0.0-SNAPSHOT/maven-metadata.xml",
	} {
		_, err := parse(t, p)
		if !errors.Is(err, api.ErrNotFound) {
			t.Fatalf("%s: err = %v, want ErrNotFound", p, err)
		}
		if !strings.Contains(err.Error(), "SNAPSHOT") || !strings.Contains(err.Error(), "Phase 5") {
			t.Errorf("%s: error %q is not a clear SNAPSHOT message", p, err)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, p := range []string{"/", "/x", "/a/b", "/a/b/c", "/a/../b/c/d.jar", "/a//b/c/d.jar"} {
		if _, err := parse(t, p); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", p)
		}
	}
}

// Golden test: Maven metadata passes through byte-identical (no URLs to
// rewrite in this format).
func TestRewriteMetadataGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "maven-metadata.xml"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Module{}.RewriteMetadata(api.Feed{Name: "central"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "maven-metadata.golden.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(golden) {
		t.Errorf("RewriteMetadata output differs from golden:\n%s", got)
	}
}
