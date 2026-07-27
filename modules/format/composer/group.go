package composer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
)

// Merging for groups. Composer learns what versions exist from the p2
// document, so that is what has to be merged; the root manifest is
// discovery, and a group answers it with its own endpoints rather than a
// member's. Dists are exact paths and stay first-hit.

// MergeableIntent implements api.GroupMerger.
func (Module) MergeableIntent(intent api.Intent) bool {
	return intent.Kind == api.IntentMetadata
}

// Merge implements api.GroupMerger.
func (m Module) Merge(feed api.Feed, intent api.Intent, parts []api.GroupPart) ([]byte, error) {
	if intent.Coord.Name == "packages.json" {
		return m.mergeRoot(feed, parts)
	}
	return m.mergePackage(feed, parts)
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
	// RewriteMetadata already knows how a root manifest must look for a
	// feed; the group is a feed as far as that is concerned.
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode composer root manifest: %w", err)
	}
	return m.RewriteMetadata(feed, body)
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
