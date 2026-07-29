package oci

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
)

// Merging for groups.
//
// A manifest and a blob are exact lookups and stay first-hit: member order
// is the operator saying whose copy of a name wins. The two listing
// endpoints are not — a group whose first member answered "these are the
// tags" would hide every tag the other members have, and `docker pull
// group/image:v2` would then fail for an image the group demonstrably
// serves.

// MergeableIntent implements api.GroupMerger.
func (Module) MergeableIntent(intent api.Intent) bool {
	return intent.Kind == api.IntentSearch
}

// Merge implements api.GroupMerger.
func (Module) Merge(_ api.Feed, intent api.Intent, parts []api.GroupPart) ([]byte, error) {
	if strings.HasSuffix(intent.RemotePath, "/_catalog") {
		repos, err := mergeStrings(parts, "repositories")
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"repositories": repos})
	}
	tags, err := mergeStrings(parts, "tags")
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"name": intent.Coord.Name, "tags": tags})
}

// mergeStrings unions the named list across members, in the lexical order
// this protocol's listings are in.
func mergeStrings(parts []api.GroupPart, field string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, part := range parts {
		if len(part.Body) == 0 {
			continue
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(part.Body, &doc); err != nil {
			return nil, fmt.Errorf("parse listing from %s: %w", part.Feed, err)
		}
		raw, ok := doc[field]
		if !ok {
			continue
		}
		var values []string
		if err := json.Unmarshal(raw, &values); err != nil {
			// A member with nothing answers with null rather than a list.
			continue
		}
		for _, v := range values {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out, nil
}
