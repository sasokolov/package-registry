package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sasokolov/package-registry/core/api"
)

func parse(t *testing.T, path string) (api.Intent, error) {
	t.Helper()
	return Module{}.Parse(httptest.NewRequest("GET", path, nil))
}

func TestParse(t *testing.T) {
	tests := []struct {
		path    string
		kind    api.IntentKind
		name    string
		version string
	}{
		{"/index.yaml", api.IntentMetadata, indexPath, ""},
		{"/charts/mychart-1.2.3.tgz", api.IntentArtifact, "mychart", "1.2.3"},
		{"/charts/mychart-1.2.3.tgz.prov", api.IntentArtifact, "mychart", "1.2.3"},
		// Both halves may contain a dash; the version is what starts with a
		// digit.
		{"/charts/nginx-ingress-4.1.0.tgz", api.IntentArtifact, "nginx-ingress", "4.1.0"},
		{"/charts/my-chart-1.0.0-rc.1.tgz", api.IntentArtifact, "my-chart", "1.0.0-rc.1"},
		{"/api/charts", api.IntentSearch, "", ""},
		{"/api/charts/mychart", api.IntentSearch, "mychart", ""},
	}
	for _, tc := range tests {
		intent, err := parse(t, tc.path)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.path, err)
			continue
		}
		if intent.Kind != tc.kind {
			t.Errorf("Parse(%q).Kind = %q, want %q", tc.path, intent.Kind, tc.kind)
		}
		if intent.Coord.Name != tc.name || intent.Coord.Version != tc.version {
			t.Errorf("Parse(%q) coord = %s@%s, want %s@%s",
				tc.path, intent.Coord.Name, intent.Coord.Version, tc.name, tc.version)
		}
	}
}

func TestParseRefusesNonsense(t *testing.T) {
	for _, p := range []string{"/", "/../etc/passwd", "/charts/", "/charts/a/b.tgz", "/whatever"} {
		if _, err := parse(t, p); !errors.Is(err, api.ErrNotFound) {
			t.Errorf("Parse(%q) = %v, want ErrNotFound", p, err)
		}
	}
}

// A proxied index that kept the upstream's URLs would send every client
// straight past the cache to the upstream, which is the one thing a proxy
// must not do.
func TestIndexURLsArePointedAtThisFeed(t *testing.T) {
	feed := api.Feed{
		Name: "charts", Format: "helm",
		Upstream: "https://charts.example.com", ExternalURL: "https://registry.example",
	}
	body := []byte(`
apiVersion: v1
entries:
  mychart:
    - name: mychart
      version: 1.0.0
      digest: abc
      urls:
        - https://charts.example.com/charts/mychart-1.0.0.tgz
    - name: mychart
      version: 0.9.0
      urls:
        - mychart-0.9.0.tgz
  cdnchart:
    - name: cdnchart
      version: 2.0.0
      urls:
        - https://cdn.elsewhere.example/downloads/cdnchart-2.0.0.tgz
`)

	out, err := Module{}.RewriteMetadata(feed, body)
	if err != nil {
		t.Fatalf("RewriteMetadata: %v", err)
	}
	var index chartIndex
	if err := yaml.Unmarshal(out, &index); err != nil {
		t.Fatal(err)
	}

	// Under the upstream: the path is kept, so the cache key reads like the
	// protocol path it is.
	if got := chartURLAt(index, "mychart", 0); got != "https://registry.example/helm/charts/charts/mychart-1.0.0.tgz" {
		t.Errorf("absolute upstream URL = %q", got)
	}
	// Relative to the index: charts sit beside it.
	if got := chartURLAt(index, "mychart", 1); got != "https://registry.example/helm/charts/charts/mychart-0.9.0.tgz" {
		t.Errorf("relative URL = %q", got)
	}
	// Somewhere else entirely — a CDN, a GitHub release. The location has
	// to travel, or the chart cannot be proxied at all.
	got := chartURLAt(index, "cdnchart", 0)
	if !strings.HasPrefix(got, "https://registry.example/helm/charts/"+remotePrefix) {
		t.Fatalf("third-party URL = %q, want it routed through the feed", got)
	}
	intent, err := parse(t, strings.TrimPrefix(got, "https://registry.example/helm/charts"))
	if err != nil {
		t.Fatalf("the rewritten URL does not parse back: %v", err)
	}
	if intent.RemoteURL != "https://cdn.elsewhere.example/downloads/cdnchart-2.0.0.tgz" {
		t.Errorf("round trip lost the location: %q", intent.RemoteURL)
	}
	if intent.Coord.Name != "cdnchart" || intent.Coord.Version != "2.0.0" {
		t.Errorf("round trip coord = %s@%s", intent.Coord.Name, intent.Coord.Version)
	}
}

func chartURLAt(index chartIndex, name string, i int) string {
	urls, _ := index.Entries[name][i]["urls"].([]any)
	if len(urls) == 0 {
		return ""
	}
	s, _ := urls[0].(string)
	return s
}

// A document that is not an index must not be passed through: handing the
// client the upstream's body would hand it the upstream's URLs.
func TestARepositoryThatIsNotOneIsRefused(t *testing.T) {
	_, err := Module{}.RewriteMetadata(api.Feed{Name: "charts"}, []byte("<html>404</html>"))
	if err == nil {
		t.Fatal("an HTML error page was accepted as an index")
	}
}

// Chart.yaml is the authority on what a chart is called, because the file
// name cannot be. A subchart's Chart.yaml must not be mistaken for it.
func TestChartMetadataComesFromTheArchive(t *testing.T) {
	archive := buildChart(t, map[string]string{
		"mychart/Chart.yaml":                "name: renamed\nversion: 9.9.9\ndescription: real\n",
		"mychart/charts/sub/Chart.yaml":     "name: sub\nversion: 0.0.1\n",
		"mychart/templates/deployment.yaml": "kind: Deployment\n",
	})

	meta, err := chartMetadata(archive)
	if err != nil {
		t.Fatalf("chartMetadata: %v", err)
	}
	if meta.Name != "renamed" || meta.Version != "9.9.9" {
		t.Errorf("got %s@%s, want renamed@9.9.9", meta.Name, meta.Version)
	}
	if !strings.Contains(string(meta.Raw), "description: real") {
		t.Errorf("the whole Chart.yaml was not kept: %q", meta.Raw)
	}
}

func TestAnArchiveWithoutAChartIsRefused(t *testing.T) {
	archive := buildChart(t, map[string]string{"mychart/values.yaml": "replicas: 1\n"})
	if _, err := chartMetadata(archive); !errors.Is(err, api.ErrBadRequest) {
		t.Errorf("err = %v, want ErrBadRequest", err)
	}
	if _, err := chartMetadata([]byte("not a gzip")); !errors.Is(err, api.ErrBadRequest) {
		t.Errorf("err = %v, want ErrBadRequest", err)
	}
}

// The generated index is a pure function of what is published: two sites
// holding the same charts must produce the same document, or the digest
// comparison that detects divergence is meaningless.
func TestTheGeneratedIndexIsDeterministic(t *testing.T) {
	feed := api.Feed{Name: "charts", ExternalURL: "https://registry.example"}
	charts := []storedChart{
		{Name: "a", Version: "1.0.0", Digest: "d1", Metadata: map[string]any{"description": "one"}},
		{Name: "a", Version: "1.1.0", Digest: "d2"},
		{Name: "b", Version: "0.1.0", Digest: "d3"},
	}
	now := time.Unix(0, 0)

	first, err := buildIndex(feed, charts, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildIndex(feed, charts, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two builds of the same charts produced different bytes")
	}

	var index chartIndex
	if err := yaml.Unmarshal(first, &index); err != nil {
		t.Fatal(err)
	}
	if got := str(index.Entries["a"][0]["version"]); got != "1.1.0" {
		t.Errorf("first version of a = %q, want the newest", got)
	}
	if got := chartURLAt(index, "a", 0); got != "https://registry.example/helm/charts/charts/a-1.1.0.tgz" {
		t.Errorf("generated URL = %q", got)
	}
	// 1.1.0 declared no description; it must not inherit 1.0.0's.
	if got := str(index.Entries["a"][0]["description"]); got != "" {
		t.Errorf("metadata leaked between versions: %q", got)
	}
	if got := str(index.Entries["a"][1]["description"]); got != "one" {
		t.Errorf("1.0.0 lost its own description: %q", got)
	}
}

// A group's index is the union: first-hit would let the hosted member hide
// every chart the upstream has.
func TestGroupIndexUnionsItsMembers(t *testing.T) {
	hosted := []byte("apiVersion: v1\nentries:\n  mine:\n    - name: mine\n      version: 1.0.0\n      urls: [a]\n  shared:\n    - name: shared\n      version: 2.0.0\n      urls: [mine]\n")
	proxy := []byte("apiVersion: v1\nentries:\n  theirs:\n    - name: theirs\n      version: 3.0.0\n      urls: [b]\n  shared:\n    - name: shared\n      version: 2.0.0\n      urls: [theirs]\n    - name: shared\n      version: 1.0.0\n      urls: [c]\n")

	out, err := Module{}.Merge(api.Feed{Name: "public"},
		api.Intent{Kind: api.IntentMetadata, Coord: api.PackageCoordinate{Name: indexPath}},
		[]api.GroupPart{{Feed: "hosted", Body: hosted}, {Feed: "proxy", Body: proxy}})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var index chartIndex
	if err := yaml.Unmarshal(out, &index); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mine", "theirs", "shared"} {
		if len(index.Entries[name]) == 0 {
			t.Errorf("%s vanished from the merged index", name)
		}
	}
	if len(index.Entries["shared"]) != 2 {
		t.Errorf("shared has %d versions, want both", len(index.Entries["shared"]))
	}
	// The earlier member wins a version both publish: member order is the
	// operator saying whom to trust for a name.
	if got := chartURLAt(index, "shared", 0); got != "mine" {
		t.Errorf("shared@2.0.0 came from %q, want the first member's", got)
	}
}

// buildChart writes a gzipped tar with the given files.
func buildChart(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
