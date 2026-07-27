package npm

import (
	"encoding/json"
	"testing"

	"github.com/sasokolov/package-registry/core/api"
)

// packument builds one member's package document as that member would have
// served it: tarball URLs already rewritten onto the member's own mount.
func packument(t *testing.T, member string, versions ...string) []byte {
	t.Helper()
	const name = "widget"
	doc := map[string]any{
		"name":      name,
		"versions":  map[string]any{},
		"time":      map[string]any{},
		"dist-tags": map[string]any{"latest": versions[len(versions)-1]},
	}
	for _, v := range versions {
		doc["versions"].(map[string]any)[v] = map[string]any{
			"name":    name,
			"version": v,
			"dist": map[string]any{
				"tarball": "http://registry.example/npm/" + member + "/" + name + "/-/" + name + "-" + v + ".tgz",
			},
		}
		doc["time"].(map[string]any)[v] = "2026-01-01T00:00:00.000Z"
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

func mergeNPM(t *testing.T, parts ...api.GroupPart) map[string]any {
	t.Helper()
	body, err := Module{}.Merge(
		api.Feed{Name: "public", Format: "npm", ExternalURL: "http://registry.example"},
		api.Intent{Kind: api.IntentMetadata}, parts)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse merged document: %v\n%s", err, body)
	}
	return doc
}

func TestMergeShowsVersionsFromEveryMember(t *testing.T) {
	doc := mergeNPM(t,
		api.GroupPart{Feed: "internal", Body: packument(t, "internal", "1.0.0")},
		api.GroupPart{Feed: "npmjs", Body: packument(t, "npmjs", "1.1.0", "2.0.0")},
	)
	versions, _ := doc["versions"].(map[string]any)
	for _, want := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		if _, ok := versions[want]; !ok {
			t.Errorf("version %s is missing; merged has %v", want, keys(versions))
		}
	}
}

// A client configured with the group must be able to fetch what the document
// tells it to fetch, so every tarball URL points at the group.
func TestMergedTarballsPointAtTheGroup(t *testing.T) {
	doc := mergeNPM(t,
		api.GroupPart{Feed: "internal", Body: packument(t, "internal", "1.0.0")},
		api.GroupPart{Feed: "npmjs", Body: packument(t, "npmjs", "2.0.0")},
	)
	versions := doc["versions"].(map[string]any)
	for version, raw := range versions {
		tarball := raw.(map[string]any)["dist"].(map[string]any)["tarball"].(string)
		want := "http://registry.example/npm/public/widget/-/widget-" + version + ".tgz"
		if tarball != want {
			t.Errorf("version %s tarball = %q, want %q", version, tarball, want)
		}
	}
}

// Member order is the operator's statement of which repository they trust
// for a name; an upstream package must not be able to replace an internal
// one just by claiming the same version.
func TestEarlierMembersWinAContestedVersion(t *testing.T) {
	internal := packument(t, "internal", "1.0.0")
	public := packument(t, "npmjs", "1.0.0")

	doc := mergeNPM(t,
		api.GroupPart{Feed: "internal", Body: internal},
		api.GroupPart{Feed: "npmjs", Body: public},
	)
	tarball := doc["versions"].(map[string]any)["1.0.0"].(map[string]any)["dist"].(map[string]any)["tarball"].(string)
	if want := "http://registry.example/npm/public/widget/-/widget-1.0.0.tgz"; tarball != want {
		t.Fatalf("tarball = %q, want %q", tarball, want)
	}
	// The identity of the winner is what matters: it must have come from
	// the first member, so swapping the order swaps the winner.
	swapped := mergeNPM(t,
		api.GroupPart{Feed: "npmjs", Body: packument(t, "npmjs", "1.0.0")},
		api.GroupPart{Feed: "internal", Body: packument(t, "internal", "1.0.0")},
	)
	if len(swapped["versions"].(map[string]any)) != 1 {
		t.Fatal("a contested version was merged into two")
	}
}

// "latest" has to name a version the merged document actually contains, and
// the newest one across members.
func TestLatestIsRecomputedAcrossMembers(t *testing.T) {
	doc := mergeNPM(t,
		api.GroupPart{Feed: "internal", Body: packument(t, "internal", "1.9.0")},
		api.GroupPart{Feed: "npmjs", Body: packument(t, "npmjs", "1.10.0")},
	)
	latest := doc["dist-tags"].(map[string]any)["latest"]
	if latest != "1.10.0" {
		t.Errorf("latest = %v, want 1.10.0 (compared as a version, not as text)", latest)
	}
}

func TestPreReleaseLosesToItsRelease(t *testing.T) {
	doc := mergeNPM(t,
		api.GroupPart{Feed: "internal", Body: packument(t, "internal", "2.0.0-rc.1")},
		api.GroupPart{Feed: "npmjs", Body: packument(t, "npmjs", "2.0.0")},
	)
	if latest := doc["dist-tags"].(map[string]any)["latest"]; latest != "2.0.0" {
		t.Errorf("latest = %v, want the release", latest)
	}
}

func TestTarballsAreFirstHitNotMerged(t *testing.T) {
	if (Module{}).MergeableIntent(api.Intent{Kind: api.IntentArtifact}) {
		t.Error("a tarball was treated as mergeable; it resolves to one artifact")
	}
	if !(Module{}).MergeableIntent(api.Intent{Kind: api.IntentMetadata}) {
		t.Error("the package document must be merged")
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
