package helm

import (
	"encoding/json"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/modules/internal/semver"
)

// Merging for groups.
//
// index.yaml is the one document that says what a repository has, so
// first-hit on it would be silently wrong: the hosted member answers, and
// every chart the upstream had disappears from `helm search`. The archives
// are exact paths and stay first-hit, which is what makes member order mean
// "whose chart wins when both have this name and version".

// MergeableIntent implements api.GroupMerger.
func (Module) MergeableIntent(intent api.Intent) bool {
	return intent.Kind == api.IntentMetadata || intent.Kind == api.IntentSearch
}

// Merge implements api.GroupMerger.
func (m Module) Merge(_ api.Feed, intent api.Intent, parts []api.GroupPart) ([]byte, error) {
	if intent.Kind == api.IntentSearch {
		return mergeListing(parts)
	}
	return mergeIndex(parts)
}

// mergeIndex unions the entries of every member's index.
//
// Earlier members win a name+version that two members both publish: member
// order is the operator's statement of which repository they trust for a
// chart, and a group that silently preferred the upstream's copy of a name
// the site hosts would be a supply-chain surprise.
func mergeIndex(parts []api.GroupPart) ([]byte, error) {
	merged := chartIndex{
		APIVersion: "v1",
		Entries:    map[string][]map[string]any{},
	}
	seen := map[string]bool{} // name@version

	for _, part := range parts {
		var index chartIndex
		if err := yaml.Unmarshal(part.Body, &index); err != nil {
			return nil, fmt.Errorf("parse helm index from %s: %w", part.Feed, err)
		}
		if merged.APIVersion == "" {
			merged.APIVersion = index.APIVersion
		}
		for name, versions := range index.Entries {
			for _, entry := range versions {
				key := name + "@" + str(entry["version"])
				if seen[key] {
					continue
				}
				seen[key] = true
				merged.Entries[name] = append(merged.Entries[name], entry)
			}
		}
	}

	for name := range merged.Entries {
		versions := merged.Entries[name]
		sort.SliceStable(versions, func(i, j int) bool {
			return semver.Compare(str(versions[i]["version"]), str(versions[j]["version"])) > 0
		})
	}
	out, err := yaml.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encode merged helm index: %w", err)
	}
	return out, nil
}

// mergeListing combines the /api/charts answers of the members.
func mergeListing(parts []api.GroupPart) ([]byte, error) {
	// The endpoint has two shapes: a map of name -> versions for
	// /api/charts, and a list of versions for /api/charts/{name}. They are
	// merged the same way, keeping the first member's answer for a version
	// two members both have.
	asMap := map[string][]map[string]any{}
	asList := []map[string]any{}
	isList := false
	seen := map[string]bool{}

	for _, part := range parts {
		if len(part.Body) == 0 {
			continue
		}
		var entries []map[string]any
		if err := json.Unmarshal(part.Body, &entries); err == nil {
			isList = true
			for _, entry := range entries {
				key := str(entry["name"]) + "@" + str(entry["version"])
				if seen[key] {
					continue
				}
				seen[key] = true
				asList = append(asList, entry)
			}
			continue
		}
		var byName map[string][]map[string]any
		if err := json.Unmarshal(part.Body, &byName); err != nil {
			return nil, fmt.Errorf("parse chart listing from %s: %w", part.Feed, err)
		}
		for name, versions := range byName {
			for _, entry := range versions {
				key := name + "@" + str(entry["version"])
				if seen[key] {
					continue
				}
				seen[key] = true
				asMap[name] = append(asMap[name], entry)
			}
		}
	}

	if isList {
		sort.SliceStable(asList, func(i, j int) bool {
			return semver.Compare(str(asList[i]["version"]), str(asList[j]["version"])) > 0
		})
		return json.Marshal(asList)
	}
	for name := range asMap {
		versions := asMap[name]
		sort.SliceStable(versions, func(i, j int) bool {
			return semver.Compare(str(versions[i]["version"]), str(versions[j]["version"])) > 0
		})
	}
	return json.Marshal(asMap)
}
