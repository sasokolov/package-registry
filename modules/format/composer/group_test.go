package composer

import (
	"encoding/json"
	"testing"

	"github.com/fondaco-dev/fondaco/core/api"
)

// available-packages is a promise of completeness: Composer will not look up
// a name that is not on it. A hosted feed can make that promise; a proxy
// cannot, and does not publish the key at all. Keeping the hosted member's
// list makes the group claim that list is everything, and every proxied
// package becomes "could not be found in any version" while sitting one
// member away.
func TestAGroupDoesNotInheritOneMembersInventory(t *testing.T) {
	hosted := `{"metadata-url":"/p2/%package%.json","available-packages":["acme/lib-a","acme/lib-b"]}`
	proxy := `{"metadata-url":"/p2/%package%.json"}`

	body, err := Module{}.Merge(
		api.Feed{Name: "composer-public", ExternalURL: "https://registry.example"},
		api.Intent{Kind: api.IntentMetadata, Coord: api.PackageCoordinate{Name: "packages.json"}},
		[]api.GroupPart{
			{Feed: "composer-hosted", Body: []byte(hosted)},
			{Feed: "packagist", Body: []byte(proxy)},
		})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if _, claimed := doc["available-packages"]; claimed {
		t.Errorf("the group claims a complete inventory it does not have: %s", body)
	}
}

// When every member does enumerate, the group can too — and the answer is
// their union, not the first one's.
func TestAGroupOfEnumeratedMembersUnionsTheirInventories(t *testing.T) {
	one := `{"metadata-url":"/p2/%package%.json","available-packages":["acme/lib-b","acme/lib-a"]}`
	two := `{"metadata-url":"/p2/%package%.json","available-packages":["acme/lib-a","acme/lib-c"]}`

	body, err := Module{}.Merge(
		api.Feed{Name: "composer-public", ExternalURL: "https://registry.example"},
		api.Intent{Kind: api.IntentMetadata, Coord: api.PackageCoordinate{Name: "packages.json"}},
		[]api.GroupPart{
			{Feed: "one", Body: []byte(one)},
			{Feed: "two", Body: []byte(two)},
		})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	var doc struct {
		Available []string `json:"available-packages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	want := []string{"acme/lib-a", "acme/lib-b", "acme/lib-c"}
	if len(doc.Available) != len(want) {
		t.Fatalf("available-packages = %v, want %v", doc.Available, want)
	}
	for i, name := range want {
		if doc.Available[i] != name {
			t.Fatalf("available-packages = %v, want %v", doc.Available, want)
		}
	}
}
