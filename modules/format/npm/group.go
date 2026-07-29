package npm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fondaco-dev/fondaco/core/api"
)

// Merging for groups. The package document ("packument") is the only thing
// npm consults to learn what versions exist, so a group that answered it
// from the first member that had the package would hide every version the
// other members hold. Tarballs are exact paths and resolve to one artifact,
// so those stay first-hit.

// MergeableIntent implements api.GroupMerger.
func (Module) MergeableIntent(intent api.Intent) bool {
	return intent.Kind == api.IntentMetadata || intent.Kind == api.IntentSearch
}

// Merge implements api.GroupMerger.
//
// Earlier members win a version that two members both publish: member order
// is the operator's statement of which repository they trust for a name, and
// that is exactly the defence against a public registry shadowing an
// internal package. Tarball URLs are re-pointed at the group, because a
// client that was configured with the group must be able to fetch what the
// document tells it to fetch.
func (Module) Merge(feed api.Feed, intent api.Intent, parts []api.GroupPart) ([]byte, error) {
	if intent.Kind == api.IntentSearch {
		return mergeSearch(parts)
	}
	return mergePackument(feed, parts)
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
	objects := []any{}

	for _, part := range parts {
		var doc struct {
			Objects []map[string]any `json:"objects"`
		}
		if err := json.Unmarshal(part.Body, &doc); err != nil {
			return nil, fmt.Errorf("parse search results from %s: %w", part.Feed, err)
		}
		for _, object := range doc.Objects {
			pkg, _ := object["package"].(map[string]any)
			name, _ := pkg["name"].(string)
			if name != "" && seen[name] {
				continue
			}
			seen[name] = true
			objects = append(objects, object)
		}
	}

	// total describes this answer. Summing the members' counts would promise
	// pages that do not exist once duplicates are removed.
	out, err := json.Marshal(map[string]any{"objects": objects, "total": len(objects)})
	if err != nil {
		return nil, fmt.Errorf("encode merged search results: %w", err)
	}
	return out, nil
}

func mergePackument(feed api.Feed, parts []api.GroupPart) ([]byte, error) {
	merged := map[string]any{}
	versions := map[string]any{}
	times := map[string]any{}
	distTags := map[string]any{}
	// origin remembers which member each version came from, so its tarball
	// URL is rewritten from the right member base.
	origin := map[string]string{}

	groupBase := feedBase(feed)

	for _, part := range parts {
		var doc map[string]any
		if err := json.Unmarshal(part.Body, &doc); err != nil {
			return nil, fmt.Errorf("parse npm package document from %s: %w", part.Feed, err)
		}

		for key, value := range doc {
			switch key {
			case "versions", "time", "dist-tags":
				continue
			default:
				// Identity and description come from the first member that
				// has the package: the earliest member is the authority.
				if _, taken := merged[key]; !taken {
					merged[key] = value
				}
			}
		}

		if fromDoc, ok := doc["versions"].(map[string]any); ok {
			for version, value := range fromDoc {
				if _, taken := versions[version]; taken {
					continue
				}
				versions[version] = value
				origin[version] = part.Feed
			}
		}
		if fromDoc, ok := doc["time"].(map[string]any); ok {
			for version, value := range fromDoc {
				if _, taken := times[version]; !taken {
					times[version] = value
				}
			}
		}
		if fromDoc, ok := doc["dist-tags"].(map[string]any); ok {
			for tag, value := range fromDoc {
				if _, taken := distTags[tag]; !taken {
					distTags[tag] = value
				}
			}
		}
	}

	if len(versions) == 0 {
		return nil, api.NotFoundf("no member of the group has any version of this package")
	}

	// Every version's tarball must be fetchable through the group.
	for version, value := range versions {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		dist, ok := entry["dist"].(map[string]any)
		if !ok {
			continue
		}
		tarball, ok := dist["tarball"].(string)
		if !ok || tarball == "" {
			continue
		}
		dist["tarball"] = repointTarball(tarball, origin[version], groupBase)
	}

	// "latest" must name a version the merged document actually has, and the
	// highest one across members — a tag copied from one member can point at
	// a version another member shadowed.
	distTags["latest"] = highestVersion(versions)

	merged["versions"] = versions
	merged["time"] = times
	merged["dist-tags"] = distTags

	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encode merged npm package document: %w", err)
	}
	return out, nil
}

// repointTarball moves a member's tarball URL onto the group, keeping the
// package path intact. The member rewrote it to its own mount point when it
// served the document; only that one segment changes.
func repointTarball(tarball, member, groupBase string) string {
	if member == "" {
		return tarball
	}
	memberBase := "/npm/" + member + "/"
	idx := strings.Index(tarball, memberBase)
	if idx < 0 {
		return tarball
	}
	return groupBase + "/" + tarball[idx+len(memberBase):]
}
