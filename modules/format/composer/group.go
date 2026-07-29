package composer

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/fondaco-dev/fondaco/core/api"
)

// Merging for groups. Composer learns what versions exist from the p2
// document, so that is what has to be merged; the root manifest is
// discovery, and a group answers it with its own endpoints rather than a
// member's. Dists are exact paths and stay first-hit.

// MergeableIntent implements api.GroupMerger.
func (Module) MergeableIntent(intent api.Intent) bool {
	return intent.Kind == api.IntentMetadata || intent.Kind == api.IntentSearch
}

// Merge implements api.GroupMerger.
func (m Module) Merge(feed api.Feed, intent api.Intent, parts []api.GroupPart) ([]byte, error) {
	switch {
	case intent.Kind == api.IntentSearch:
		return mergeSearch(parts)
	case intent.Coord.Name == "packages.json":
		return m.mergeRoot(feed, parts)
	default:
		return m.mergePackage(feed, parts)
	}
}

// mergeSearch concatenates members' results, earlier members first, keeping
// one entry per package name — the same precedence the rest of the group
// follows, so what search shows is what an install will actually get.
//
// Two rankings do not compose into a third that means anything, but the
// alternative is worse: the hosted member answers every search, so first-hit
// would mean upstream results never appear at all.
func mergeSearch(parts []api.GroupPart) ([]byte, error) {
	seen := map[string]bool{}
	results := []any{}

	for _, part := range parts {
		var doc struct {
			Results []map[string]any `json:"results"`
		}
		if err := json.Unmarshal(part.Body, &doc); err != nil {
			return nil, fmt.Errorf("parse search results from %s: %w", part.Feed, err)
		}
		for _, result := range doc.Results {
			name, _ := result["name"].(string)
			if name != "" && seen[name] {
				continue
			}
			seen[name] = true
			results = append(results, result)
		}
	}

	// total describes this answer. Summing the members' counts would promise
	// pages that do not exist once duplicates are removed.
	out, err := json.Marshal(map[string]any{"results": results, "total": len(results)})
	if err != nil {
		return nil, fmt.Errorf("encode merged search results: %w", err)
	}
	return out, nil
}

// mergeRoot answers discovery for the group itself. The members' root
// manifests differ only in the endpoints they advertise, and every one of
// those points at a member; taking the first would send the client away from
// the group it was configured with.
func (m Module) mergeRoot(feed api.Feed, parts []api.GroupPart) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(parts[0].Body, &doc); err != nil {
		return nil, fmt.Errorf("parse composer root manifest from %s: %w", parts[0].Feed, err)
	}
	if err := mergeInventory(doc, parts); err != nil {
		return nil, err
	}
	// RewriteMetadata already knows how a root manifest must look for a
	// feed; the group is a feed as far as that is concerned.
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode composer root manifest: %w", err)
	}
	return m.RewriteMetadata(feed, body)
}

// inventoryKeys are the root manifest's exhaustive lists: Composer will not
// ask for a package name that is not covered by them.
var inventoryKeys = []string{"available-packages", "available-package-patterns"}

// mergeInventory reconciles the members' claims about what they contain.
//
// These lists are a promise of completeness, and a hosted feed makes it
// because it knows everything it holds. A proxy does not publish one at all,
// because it can serve whatever its upstream has. Taking the first member's
// list — which is what merging the first document alone amounts to — makes
// the group claim the hosted feed's inventory is the whole group, and
// Composer then refuses to look up any proxied package: "could not be found
// in any version", for a package sitting one member away.
//
// So the list survives only if every member that answered made the promise,
// and then it is their union.
func mergeInventory(doc map[string]any, parts []api.GroupPart) error {
	unions := map[string][]string{}
	complete := map[string]bool{}
	for _, key := range inventoryKeys {
		complete[key] = true
	}

	for _, part := range parts {
		var member map[string]any
		if err := json.Unmarshal(part.Body, &member); err != nil {
			return fmt.Errorf("parse composer root manifest from %s: %w", part.Feed, err)
		}
		for _, key := range inventoryKeys {
			listed, ok := member[key].([]any)
			if !ok {
				complete[key] = false
				continue
			}
			for _, raw := range listed {
				if name, ok := raw.(string); ok {
					unions[key] = append(unions[key], name)
				}
			}
		}
	}

	for _, key := range inventoryKeys {
		if !complete[key] {
			delete(doc, key)
			continue
		}
		names := unions[key]
		sort.Strings(names)
		doc[key] = slices.Compact(names)
	}
	return nil
}

// mergePackage unions the version lists of one package across members.
//
// Earlier members win a version two members both publish: member order is
// the operator's statement of which repository they trust for a name.
func (Module) mergePackage(feed api.Feed, parts []api.GroupPart) ([]byte, error) {
	base := feedBase(feed)
	merged := map[string]any{}
	packages := map[string]any{}
	// seen is per package name, because one document can carry several.
	seen := map[string]map[string]bool{}
	order := map[string][]any{}
	names := []string{}

	for _, part := range parts {
		var doc map[string]any
		if err := json.Unmarshal(part.Body, &doc); err != nil {
			return nil, fmt.Errorf("parse composer package document from %s: %w", part.Feed, err)
		}
		for key, value := range doc {
			if key == "packages" {
				continue
			}
			if _, taken := merged[key]; !taken {
				merged[key] = value
			}
		}

		fromDoc, ok := doc["packages"].(map[string]any)
		if !ok {
			continue
		}
		for name, raw := range fromDoc {
			versions, ok := raw.([]any)
			if !ok {
				continue
			}
			if seen[name] == nil {
				seen[name] = map[string]bool{}
				names = append(names, name)
			}
			for _, item := range versions {
				entry, ok := item.(map[string]any)
				if !ok {
					continue
				}
				version, _ := entry["version"].(string)
				if version != "" {
					if seen[name][version] {
						continue
					}
					seen[name][version] = true
				}
				repointDist(entry, part.Feed, base)
				order[name] = append(order[name], entry)
			}
		}
	}

	if len(names) == 0 {
		return nil, api.NotFoundf("no member of the group has this package")
	}
	for _, name := range names {
		packages[name] = order[name]
	}
	merged["packages"] = packages

	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encode merged composer package document: %w", err)
	}
	return out, nil
}

// repointDist moves a version's dist URL from the member it came from onto
// the group, so a client configured with the group can fetch it.
func repointDist(entry map[string]any, member, groupBase string) {
	dist, ok := entry["dist"].(map[string]any)
	if !ok {
		return
	}
	distURL, ok := dist["url"].(string)
	if !ok || distURL == "" {
		return
	}
	memberBase := "/composer/" + member + "/"
	idx := strings.Index(distURL, memberBase)
	if idx < 0 {
		return
	}
	dist["url"] = groupBase + "/" + distURL[idx+len(memberBase):]
}
