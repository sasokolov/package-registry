package maven

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/fondaco-dev/fondaco/core/api"
)

// metadataDoc is one member's maven-metadata.xml for com.example:lib.
func metadataDoc(t *testing.T, versions ...string) []byte {
	t.Helper()
	doc := mavenMetadata{
		GroupID: "com.example", ArtifactID: "lib",
		Versioning: versioning{
			Latest:      versions[len(versions)-1],
			Release:     latestRelease(versions),
			Versions:    versionList(versions),
			LastUpdated: "20260101000000",
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append([]byte(xml.Header), body...)
}

func versionList(v []string) versions { return versions{Version: v} }

func parseMerged(t *testing.T, body []byte) mavenMetadata {
	t.Helper()
	var doc mavenMetadata
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse merged document: %v\n%s", err, body)
	}
	return doc
}

// The whole point of a group: a hosted member must not hide what the proxied
// member has, and vice versa.
func TestMergeUnionsVersionsFromEveryMember(t *testing.T) {
	merged, err := Module{}.Merge(api.Feed{Name: "public", Format: "maven"}, api.Intent{}, []api.GroupPart{
		{Feed: "releases", Body: metadataDoc(t, "1.0.0", "2.0.0")},
		{Feed: "central", Body: metadataDoc(t, "1.5.0", "3.0.0")},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	doc := parseMerged(t, merged)
	got := strings.Join(doc.Versioning.Versions.Version, ",")
	if want := "1.0.0,1.5.0,2.0.0,3.0.0"; got != want {
		t.Errorf("versions = %q, want %q", got, want)
	}
	if doc.GroupID != "com.example" || doc.ArtifactID != "lib" {
		t.Errorf("identity lost: %s:%s", doc.GroupID, doc.ArtifactID)
	}
}

// latest and release describe the merged set, not whichever member happened
// to answer first: a member's own "latest" is only latest within it.
func TestMergeRecomputesLatestAcrossMembers(t *testing.T) {
	merged, err := Module{}.Merge(api.Feed{Name: "public"}, api.Intent{}, []api.GroupPart{
		{Feed: "releases", Body: metadataDoc(t, "9.0.0")},
		{Feed: "central", Body: metadataDoc(t, "10.0.0")},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	doc := parseMerged(t, merged)
	if doc.Versioning.Latest != "10.0.0" {
		t.Errorf("latest = %q, want 10.0.0 (a version comparison, not a string one)", doc.Versioning.Latest)
	}
	if doc.Versioning.Release != "10.0.0" {
		t.Errorf("release = %q, want 10.0.0", doc.Versioning.Release)
	}
}

func TestMergeKeepsSnapshotsOutOfRelease(t *testing.T) {
	merged, err := Module{}.Merge(api.Feed{Name: "public"}, api.Intent{}, []api.GroupPart{
		{Feed: "releases", Body: metadataDoc(t, "1.0.0", "1.1.0-SNAPSHOT")},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	doc := parseMerged(t, merged)
	if doc.Versioning.Release != "1.0.0" {
		t.Errorf("release = %q, want the newest non-SNAPSHOT", doc.Versioning.Release)
	}
	if doc.Versioning.Latest != "1.1.0-SNAPSHOT" {
		t.Errorf("latest = %q, want the newest version of any kind", doc.Versioning.Latest)
	}
}

// The same version in two members is one version, not two.
func TestMergeDeduplicates(t *testing.T) {
	merged, err := Module{}.Merge(api.Feed{Name: "public"}, api.Intent{}, []api.GroupPart{
		{Feed: "a", Body: metadataDoc(t, "1.0.0", "2.0.0")},
		{Feed: "b", Body: metadataDoc(t, "2.0.0")},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	doc := parseMerged(t, merged)
	if len(doc.Versioning.Versions.Version) != 2 {
		t.Errorf("versions = %v, want two", doc.Versioning.Versions.Version)
	}
}

// Merging twice must give the same bytes: two replicas answer the same
// request, and a client that compares them must not see a difference.
func TestMergeIsDeterministic(t *testing.T) {
	parts := []api.GroupPart{
		{Feed: "a", Body: metadataDoc(t, "1.0.0", "3.0.0")},
		{Feed: "b", Body: metadataDoc(t, "2.0.0")},
	}
	first, err := Module{}.Merge(api.Feed{Name: "public"}, api.Intent{}, parts)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := Module{}.Merge(api.Feed{Name: "public"}, api.Intent{}, parts)
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if string(again) != string(first) {
			t.Fatalf("merge %d differs:\n%s\n---\n%s", i, first, again)
		}
	}
}

func TestOnlyTheIndexIsMerged(t *testing.T) {
	tests := []struct {
		name   string
		intent api.Intent
		want   bool
	}{
		{
			name:   "the artifact index is merged",
			intent: api.Intent{Kind: api.IntentMetadata, RemotePath: "com/example/lib/maven-metadata.xml"},
			want:   true,
		},
		{
			name:   "a jar resolves to one artifact",
			intent: api.Intent{Kind: api.IntentArtifact, RemotePath: "com/example/lib/1.0/lib-1.0.jar"},
			want:   false,
		},
		{
			name: "the index checksum describes one member's copy",
			intent: api.Intent{
				Kind: api.IntentMetadata, RemotePath: "com/example/lib/maven-metadata.xml",
				WantChecksum: "sha1",
			},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Module{}).MergeableIntent(tc.intent); got != tc.want {
				t.Errorf("MergeableIntent = %v, want %v", got, tc.want)
			}
		})
	}
}
