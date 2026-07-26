package maven

import (
	"strings"
	"testing"

	"github.com/sasokolov/package-registry/core/api"
)

func snapshotManifest(path, version string) api.HostedManifest {
	return api.HostedManifest{
		Path:  path,
		Coord: api.PackageCoordinate{Format: "maven", Name: "com.example:lib", Version: version},
	}
}

func TestReindexSnapshots(t *testing.T) {
	core := newFakeCore()
	base := "com/example/lib/1.0.0-SNAPSHOT/"
	manifests := []api.HostedManifest{
		snapshotManifest(base+"lib-1.0.0-20260726.101500-1.jar", "1.0.0-SNAPSHOT"),
		snapshotManifest(base+"lib-1.0.0-20260726.101500-1.pom", "1.0.0-SNAPSHOT"),
		snapshotManifest(base+"lib-1.0.0-20260726.120000-2.jar", "1.0.0-SNAPSHOT"),
		snapshotManifest(base+"lib-1.0.0-20260726.120000-2.pom", "1.0.0-SNAPSHOT"),
	}
	feed := api.Feed{Name: "hosted", Format: "maven"}
	if err := reindexSnapshots(t.Context(), feed, core, manifests, 5); err != nil {
		t.Fatalf("reindexSnapshots: %v", err)
	}
	doc := string(core.indexes[base+"maven-metadata.xml"])
	if doc == "" {
		t.Fatal("no snapshot metadata generated")
	}
	// The newest build wins.
	if !strings.Contains(doc, "<timestamp>20260726.120000</timestamp>") ||
		!strings.Contains(doc, "<buildNumber>2</buildNumber>") {
		t.Errorf("newest build not selected:\n%s", doc)
	}
	// One snapshotVersion entry per extension, pointing at the timestamped file.
	if !strings.Contains(doc, "<extension>jar</extension>") ||
		!strings.Contains(doc, "<extension>pom</extension>") {
		t.Errorf("missing per-extension entries:\n%s", doc)
	}
	if !strings.Contains(doc, "<value>1.0.0-20260726.120000-2</value>") {
		t.Errorf("value does not name the newest build:\n%s", doc)
	}

	// Deterministic: same input, same bytes.
	first := doc
	if err := reindexSnapshots(t.Context(), feed, core, manifests, 5); err != nil {
		t.Fatal(err)
	}
	if first != string(core.indexes[base+"maven-metadata.xml"]) {
		t.Error("snapshot reindex is not deterministic")
	}
}

func TestSnapshotRetention(t *testing.T) {
	core := newFakeCore()
	base := "com/example/lib/2.0.0-SNAPSHOT/"
	var manifests []api.HostedManifest
	for _, b := range []struct {
		stamp string
		num   int
	}{{"20260701.100000", 1}, {"20260702.100000", 2}, {"20260703.100000", 3}, {"20260704.100000", 4}} {
		manifests = append(manifests, snapshotManifest(
			base+"lib-2.0.0-"+b.stamp+"-"+string(rune('0'+b.num))+".jar", "2.0.0-SNAPSHOT"))
	}
	feed := api.Feed{Name: "hosted", Format: "maven"}
	if err := reindexSnapshots(t.Context(), feed, core, manifests, 2); err != nil {
		t.Fatal(err)
	}
	doc := string(core.indexes[base+"maven-metadata.xml"])
	// Retention keeps the newest builds; the index names the newest one.
	if !strings.Contains(doc, "<buildNumber>4</buildNumber>") {
		t.Errorf("newest build missing:\n%s", doc)
	}
	if strings.Contains(doc, "20260701.100000") || strings.Contains(doc, "20260702.100000") {
		t.Errorf("retention did not drop old builds:\n%s", doc)
	}
}

func TestKindOf(t *testing.T) {
	tests := map[string]string{
		"lib-1.0.0-20260726.101500-3.jar":         "jar",
		"lib-1.0.0-20260726.101500-3.pom":         "pom",
		"lib-1.0.0-20260726.101500-3-sources.jar": "sources.jar",
	}
	for file, want := range tests {
		match := timestampedRE.FindStringSubmatch(file)
		if match == nil {
			t.Fatalf("%s did not match the timestamp pattern", file)
		}
		// The regex match includes the trailing separator; kindOf trims it.
		stamp := "-" + match[1] + "-" + match[2]
		if got := kindOf(file, stamp); got != want {
			t.Errorf("kindOf(%q) = %q, want %q", file, got, want)
		}
	}
}
