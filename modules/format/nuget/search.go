package nuget

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/fondaco-dev/fondaco/core/api"
)

// Search over a hosted feed.
//
// This is the one read a registry cannot precompute. An index enumerates
// what exists and is rebuilt when the content changes; a search depends on
// what was asked. So it is answered per request, from the same facts the
// registration documents are built from — which keeps one source of truth
// and means a search can never disagree with a restore.
//
// The ranking is deliberately plain: an exact id first, then a prefix match,
// then anything containing the term, alphabetically within each. A hosted
// feed holds what one organisation published; inventing relevance scoring
// over that would be pretending to solve a problem it does not have.

// defaultTake matches what the NuGet client asks for when it does not say.
const defaultTake = 20

// maxTake bounds a page: an unbounded take is a way to ask one request to
// render the whole feed.
const maxTake = 1000

// Search implements api.Searcher.
func (Module) Search(ctx context.Context, feed api.Feed, intent api.Intent, deps api.CoreServices) (api.SyntheticResponse, error) {
	params, err := url.ParseQuery(intent.RemoteQuery)
	if err != nil {
		return api.SyntheticResponse{}, fmt.Errorf("invalid search query: %v: %w", err, api.ErrBadRequest)
	}
	term := strings.ToLower(strings.TrimSpace(params.Get("q")))
	prerelease := params.Get("prerelease") == "true"
	skip := intParam(params.Get("skip"), 0)
	take := intParam(params.Get("take"), defaultTake)
	if take > maxTake {
		take = maxTake
	}

	packages, err := hostedPackages(ctx, feed, deps)
	if err != nil {
		return api.SyntheticResponse{}, err
	}

	base := feedBase(feed)
	matches := make([]map[string]any, 0, len(packages))
	for _, lid := range sortedIDs(packages) {
		versions := packages[lid]
		entry, ok := searchEntry(base, lid, versions, prerelease)
		if !ok {
			continue
		}
		if term != "" && !matchesTerm(entry, term) {
			continue
		}
		matches = append(matches, entry)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return rank(matches[i], term) < rank(matches[j], term)
	})

	total := len(matches)
	if skip > total {
		skip = total
	}
	end := skip + take
	if end > total {
		end = total
	}

	body, err := json.MarshalIndent(map[string]any{
		"@context":  map[string]any{"@vocab": "http://schema.nuget.org/schema#"},
		"totalHits": total,
		"data":      matches[skip:end],
	}, "", "  ")
	if err != nil {
		return api.SyntheticResponse{}, fmt.Errorf("encode search results: %w", err)
	}
	return api.SyntheticResponse{
		Status: http.StatusOK,
		Header: map[string]string{"Content-Type": "application/json"},
		Body:   append(body, '\n'),
	}, nil
}

// hostedPackages reads the published facts, grouped by lowercased id.
func hostedPackages(ctx context.Context, feed api.Feed, deps api.CoreServices) (map[string][]hostedVersion, error) {
	manifests, err := deps.Manifests(ctx, feed, hostedStatePrefix)
	if err != nil {
		return nil, err
	}
	out := map[string][]hostedVersion{}
	for _, m := range manifests {
		body, err := readBlob(ctx, deps, m.SHA256)
		if err != nil {
			return nil, err
		}
		var state hostedVersion
		if err := json.Unmarshal(body, &state); err != nil {
			return nil, fmt.Errorf("parse package facts at %s: %w", m.Path, err)
		}
		lid := strings.ToLower(state.ID)
		out[lid] = append(out[lid], state)
	}
	return out, nil
}

func sortedIDs(packages map[string][]hostedVersion) []string {
	ids := make([]string, 0, len(packages))
	for id := range packages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// searchEntry renders one package: its newest matching version, with every
// version listed beside it the way the NuGet client expects.
func searchEntry(base, lid string, versions []hostedVersion, prerelease bool) (map[string]any, bool) {
	sort.Slice(versions, func(i, j int) bool {
		return compareNuGetVersions(versions[i].Version, versions[j].Version) < 0
	})

	rendered := make([]any, 0, len(versions))
	var newest *hostedVersion
	for i := range versions {
		v := versions[i]
		lver := strings.ToLower(v.Version)
		rendered = append(rendered, map[string]any{
			"@id": base + "/v3/registration/" + lid + "/" + lver + ".json",
			// A hosted feed does not count downloads; reporting a number it
			// did not measure would be worse than reporting none.
			"downloads": 0,
			"version":   v.Version,
		})
		if isPreRelease(v.Version) && !prerelease {
			continue
		}
		newest = &versions[i]
	}
	if newest == nil {
		return nil, false
	}

	entry := map[string]any{
		"@id":            base + "/v3/registration/" + lid + "/index.json",
		"@type":          "Package",
		"registration":   base + "/v3/registration/" + lid + "/index.json",
		"id":             newest.ID,
		"version":        newest.Version,
		"versions":       rendered,
		"totalDownloads": 0,
	}
	if newest.Description != "" {
		entry["description"] = newest.Description
	}
	if newest.Authors != "" {
		entry["authors"] = []string{newest.Authors}
	}
	if newest.License != "" {
		entry["licenseExpression"] = newest.License
	}
	if newest.LicenseURL != "" {
		entry["licenseUrl"] = newest.LicenseURL
	}
	if newest.ProjectURL != "" {
		entry["projectUrl"] = newest.ProjectURL
	}
	return entry, true
}

// matchesTerm looks where a person would expect a search to look.
func matchesTerm(entry map[string]any, term string) bool {
	for _, key := range []string{"id", "description"} {
		if value, ok := entry[key].(string); ok && strings.Contains(strings.ToLower(value), term) {
			return true
		}
	}
	if authors, ok := entry["authors"].([]string); ok {
		for _, a := range authors {
			if strings.Contains(strings.ToLower(a), term) {
				return true
			}
		}
	}
	return false
}

// rank orders results: exact id, then id prefix, then everything else.
func rank(entry map[string]any, term string) int {
	id, _ := entry["id"].(string)
	lid := strings.ToLower(id)
	switch {
	case term == "":
		return 1
	case lid == term:
		return 0
	case strings.HasPrefix(lid, term):
		return 1
	default:
		return 2
	}
}

func isPreRelease(version string) bool { return strings.Contains(version, "-") }

func intParam(raw string, def int) int {
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return def
	}
	return v
}
