package terraform

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/fondaco-dev/fondaco/core/api"
)

// Merging for groups. The versions document is the only thing Terraform
// consults to learn which versions of a module exist, so a group that
// answered it from the first member that had the module would hide every
// version the others hold — and for Terraform that matters more than for
// most formats, because a module source names a HOST. Without a group there
// is no way to serve private modules and a proxied registry from the same
// address at all.
//
// Archives resolve to one artifact and stay first-hit.

// MergeableIntent implements api.GroupMerger.
func (Module) MergeableIntent(intent api.Intent) bool {
	return intent.Kind == api.IntentMetadata
}

// Merge implements api.GroupMerger: the union of the members' versions, per
// module source.
//
// Earlier members win a version two of them both publish, the same
// precedence the rest of the group follows, so what the list shows is what a
// download will actually get.
func (Module) Merge(_ api.Feed, _ api.Intent, parts []api.GroupPart) ([]byte, error) {
	// A source may be listed by several members; versions are a set per
	// source, and the source order follows first appearance so the answer is
	// stable.
	seen := map[string]map[string]bool{}
	var order []string

	for _, part := range parts {
		var doc versionsDoc
		if err := json.Unmarshal(part.Body, &doc); err != nil {
			return nil, fmt.Errorf("parse versions document from %s: %w", part.Feed, err)
		}
		for _, module := range doc.Modules {
			if seen[module.Source] == nil {
				seen[module.Source] = map[string]bool{}
				order = append(order, module.Source)
			}
			for _, v := range module.Versions {
				if v.Version != "" {
					seen[module.Source][v.Version] = true
				}
			}
		}
	}

	if len(order) == 0 {
		return nil, api.NotFoundf("no member of the group lists this module")
	}

	merged := versionsDoc{Modules: make([]versionsModule, 0, len(order))}
	for _, source := range order {
		versions := make([]string, 0, len(seen[source]))
		for v := range seen[source] {
			versions = append(versions, v)
		}
		sort.Slice(versions, func(i, j int) bool {
			return compareModuleVersions(versions[i], versions[j]) < 0
		})
		entries := make([]versionsEntry, 0, len(versions))
		for _, v := range versions {
			entries = append(entries, versionsEntry{Version: v})
		}
		merged.Modules = append(merged.Modules, versionsModule{Source: source, Versions: entries})
	}

	body, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encode merged versions document: %w", err)
	}
	return body, nil
}

// compareModuleVersions orders two module versions: numeric segments as
// numbers, and a pre-release losing to its release.
func compareModuleVersions(a, b string) int {
	aCore, aPre, _ := strings.Cut(strings.TrimPrefix(a, "v"), "-")
	bCore, bPre, _ := strings.Cut(strings.TrimPrefix(b, "v"), "-")

	aSegs, bSegs := strings.Split(aCore, "."), strings.Split(bCore, ".")
	for i := 0; i < len(aSegs) || i < len(bSegs); i++ {
		if c := compareSegment(segmentAt(aSegs, i), segmentAt(bSegs, i)); c != 0 {
			return c
		}
	}
	switch {
	case aPre == "" && bPre == "":
		return 0
	case aPre == "":
		return 1
	case bPre == "":
		return -1
	}
	return strings.Compare(aPre, bPre)
}

func segmentAt(segs []string, i int) string {
	if i < len(segs) {
		return segs[i]
	}
	return "0"
}

func compareSegment(a, b string) int {
	an, aOK := atoi(a)
	bn, bOK := atoi(b)
	if aOK && bOK {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a, b)
}

func atoi(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}
