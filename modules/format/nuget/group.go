package nuget

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
)

// Merging for groups. Two documents decide what a client believes exists:
// the flat-container index (which versions of an id are available) and the
// registration index (their dependency metadata). Answering either from the
// first member that has the id would hide every version the other members
// hold, which for a package that exists both internally and upstream is
// exactly the case a group is built for.
//
// Search is merged too, though for a different reason. Concatenating two
// rankings does produce a third that means less than either — but the
// alternative is worse: the hosted member can always answer a search, so
// first-hit would mean upstream results never appear at all. Losing the
// ranking beats losing the results.

// MergeableIntent implements api.GroupMerger.
func (Module) MergeableIntent(intent api.Intent) bool {
	if intent.Kind == api.IntentSearch {
		return true
	}
	if intent.Kind != api.IntentMetadata {
		return false
	}
	switch {
	case strings.HasPrefix(intent.RemotePath, upstreamFlatPrefix) &&
		strings.HasSuffix(intent.RemotePath, "/index.json"):
		return true
	case strings.HasPrefix(intent.RemotePath, upstreamRegistrationPrefix) &&
		strings.HasSuffix(intent.RemotePath, "/index.json"):
		return true
	default:
		return false
	}
}

// Merge implements api.GroupMerger.
func (Module) Merge(_ api.Feed, intent api.Intent, parts []api.GroupPart) ([]byte, error) {
	switch {
	case intent.Kind == api.IntentSearch:
		return mergeSearch(parts)
	case strings.HasPrefix(intent.RemotePath, upstreamFlatPrefix):
		return mergeFlatIndex(parts)
	default:
		return mergeRegistrationIndex(parts)
	}
}

// mergeSearch concatenates members' results, earlier members first, keeping
// one entry per package id — the same precedence rule the rest of the group
// follows, so what search shows is what a restore will actually get.
func mergeSearch(parts []api.GroupPart) ([]byte, error) {
	seen := map[string]bool{}
	data := []any{}

	for _, part := range parts {
		var doc struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(part.Body, &doc); err != nil {
			return nil, fmt.Errorf("parse search results from %s: %w", part.Feed, err)
		}
		for _, entry := range doc.Data {
			id, _ := entry["id"].(string)
			key := strings.ToLower(id)
			if key != "" && seen[key] {
				continue
			}
			seen[key] = true
			data = append(data, entry)
		}
	}

	// totalHits describes what is in this answer. Summing the members'
	// counts would promise pages that do not exist once duplicates are
	// removed.
	out, err := json.Marshal(map[string]any{
		"@context":  map[string]any{"@vocab": "http://schema.nuget.org/schema#"},
		"totalHits": len(data),
		"data":      data,
	})
	if err != nil {
		return nil, fmt.Errorf("encode merged search results: %w", err)
	}
	return out, nil
}

// mergeFlatIndex unions the version lists of one package id.
func mergeFlatIndex(parts []api.GroupPart) ([]byte, error) {
	seen := map[string]bool{}
	var all []string

	for _, part := range parts {
		var doc struct {
			Versions []string `json:"versions"`
		}
		if err := json.Unmarshal(part.Body, &doc); err != nil {
			return nil, fmt.Errorf("parse nuget flat index from %s: %w", part.Feed, err)
		}
		for _, v := range doc.Versions {
			key := strings.ToLower(v)
			if v == "" || seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, v)
		}
	}
	if len(all) == 0 {
		return nil, api.NotFoundf("no member of the group lists any version")
	}
	sort.Slice(all, func(i, j int) bool { return compareNuGetVersions(all[i], all[j]) < 0 })

	out, err := json.Marshal(map[string]any{"versions": all})
	if err != nil {
		return nil, fmt.Errorf("encode merged nuget flat index: %w", err)
	}
	return out, nil
}

// mergeRegistrationIndex concatenates the members' registration pages.
//
// Page URLs are deliberately left pointing at the member that produced them.
// Re-pointing them at the group would make the group resolve each page by
// first hit, which is the very ambiguity this merge exists to remove; and a
// member whose document is in this answer is by construction one the caller
// may read, so following its URL is safe.
func mergeRegistrationIndex(parts []api.GroupPart) ([]byte, error) {
	merged := map[string]any{}
	var items []any
	count := 0

	for _, part := range parts {
		var doc map[string]any
		if err := json.Unmarshal(part.Body, &doc); err != nil {
			return nil, fmt.Errorf("parse nuget registration index from %s: %w", part.Feed, err)
		}
		for key, value := range doc {
			if key == "items" || key == "count" {
				continue
			}
			if _, taken := merged[key]; !taken {
				merged[key] = value
			}
		}
		page, ok := doc["items"].([]any)
		if !ok {
			continue
		}
		items = append(items, page...)
		count += len(page)
	}

	if len(items) == 0 {
		return nil, api.NotFoundf("no member of the group has registration data for this package")
	}
	merged["items"] = items
	merged["count"] = count

	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encode merged nuget registration index: %w", err)
	}
	return out, nil
}

// compareNuGetVersions orders two NuGet versions: numeric segments compared
// as numbers, and a pre-release losing to its release.
func compareNuGetVersions(a, b string) int {
	aCore, aPre, _ := strings.Cut(a, "-")
	bCore, bPre, _ := strings.Cut(b, "-")

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
