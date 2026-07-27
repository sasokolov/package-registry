package terraform

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sasokolov/package-registry/core/api"
)

func versionsBody(t *testing.T, source string, versions ...string) []byte {
	t.Helper()
	doc := versionsDoc{Modules: []versionsModule{{Source: source}}}
	for _, v := range versions {
		doc.Modules[0].Versions = append(doc.Modules[0].Versions, versionsEntry{Version: v})
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

func mergeVersions(t *testing.T, parts ...api.GroupPart) versionsDoc {
	t.Helper()
	body, err := Module{}.Merge(api.Feed{Name: "public"}, api.Intent{}, parts)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var doc versionsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse merged document: %v\n%s", err, body)
	}
	return doc
}

// Without this, a site that hosts its own modules and proxies the public
// registry would show only one of them — and Terraform gives no way to
// address the other, because a module source names a host.
func TestMergeUnionsVersionsFromEveryMember(t *testing.T) {
	doc := mergeVersions(t,
		api.GroupPart{Feed: "internal", Body: versionsBody(t, "acme/vpc/aws", "1.0.0")},
		api.GroupPart{Feed: "public", Body: versionsBody(t, "acme/vpc/aws", "1.1.0", "2.0.0")},
	)
	if len(doc.Modules) != 1 {
		t.Fatalf("modules = %d, want one source", len(doc.Modules))
	}
	got := make([]string, 0, len(doc.Modules[0].Versions))
	for _, v := range doc.Modules[0].Versions {
		got = append(got, v.Version)
	}
	if want := "1.0.0,1.1.0,2.0.0"; strings.Join(got, ",") != want {
		t.Errorf("versions = %v, want %s", got, want)
	}
}

func TestMergeOrdersVersionsAsVersions(t *testing.T) {
	doc := mergeVersions(t,
		api.GroupPart{Feed: "a", Body: versionsBody(t, "acme/vpc/aws", "1.9.0", "1.10.0", "2.0.0-rc.1")},
	)
	got := make([]string, 0)
	for _, v := range doc.Modules[0].Versions {
		got = append(got, v.Version)
	}
	// 1.9.0 before 1.10.0, and the pre-release before its release line.
	if want := "1.9.0,1.10.0,2.0.0-rc.1"; strings.Join(got, ",") != want {
		t.Errorf("versions = %v, want %s", got, want)
	}
}

func TestMergeKeepsSourcesApart(t *testing.T) {
	doc := mergeVersions(t,
		api.GroupPart{Feed: "a", Body: versionsBody(t, "acme/vpc/aws", "1.0.0")},
		api.GroupPart{Feed: "b", Body: versionsBody(t, "acme/db/aws", "3.0.0")},
	)
	if len(doc.Modules) != 2 {
		t.Fatalf("modules = %d, want two sources", len(doc.Modules))
	}
}

func TestMergeIsDeterministic(t *testing.T) {
	parts := []api.GroupPart{
		{Feed: "a", Body: versionsBody(t, "acme/vpc/aws", "1.0.0", "3.0.0")},
		{Feed: "b", Body: versionsBody(t, "acme/vpc/aws", "2.0.0")},
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

func TestArchivesAreFirstHitNotMerged(t *testing.T) {
	if (Module{}).MergeableIntent(api.Intent{Kind: api.IntentArtifact}) {
		t.Error("a module archive was treated as mergeable; it resolves to one artifact")
	}
	if !(Module{}).MergeableIntent(api.Intent{Kind: api.IntentMetadata}) {
		t.Error("the versions document must be merged")
	}
}
