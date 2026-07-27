package composer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
)

// Search over a hosted feed.
//
// Answered from the same facts the p2 documents are built from, so a search
// cannot show a package a subsequent install would fail to resolve.
//
// Packagist reports download and star counts with every result. A private
// registry measures neither, so both are reported as zero rather than
// invented, and the ordering carries what meaning there is: an exact name
// first, then a prefix, then anything matching.

// searchDefaultPerPage is what `composer search` gets when it does not ask.
const searchDefaultPerPage = 15

// searchMaxPerPage bounds a page.
const searchMaxPerPage = 100

// Search implements api.Searcher.
func (Module) Search(ctx context.Context, feed api.Feed, intent api.Intent, deps api.CoreServices) (api.SyntheticResponse, error) {
	params, err := url.ParseQuery(intent.RemoteQuery)
	if err != nil {
		return api.SyntheticResponse{}, fmt.Errorf("invalid search query: %v: %w", err, api.ErrBadRequest)
	}
	term := strings.ToLower(strings.TrimSpace(params.Get("q")))
	wantType := strings.TrimSpace(params.Get("type"))
	page := intParam(params.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	perPage := intParam(params.Get("per_page"), searchDefaultPerPage)
	if perPage > searchMaxPerPage {
		perPage = searchMaxPerPage
	}

	releases, err := hostedPackages(ctx, feed, deps)
	if err != nil {
		return api.SyntheticResponse{}, err
	}

	names := make([]string, 0, len(releases))
	for name := range releases {
		names = append(names, name)
	}
	sort.Strings(names)

	base := feedBase(feed)
	results := make([]any, 0, len(names))
	for _, name := range names {
		newest := newestRelease(releases[name])
		if newest == nil {
			continue
		}
		if wantType != "" && packageType(*newest) != wantType {
			continue
		}
		if term != "" && !matchesTerm(*newest, term) {
			continue
		}
		result := map[string]any{
			"name": newest.Name,
			"url":  base + "/p2/" + newest.Name + ".json",
			// A private registry counts neither downloads nor stars.
			// Reporting zero is the truthful answer; making numbers up
			// would make the ranking a lie.
			"downloads": 0,
			"favers":    0,
		}
		if newest.Description != "" {
			result["description"] = newest.Description
		}
		if t := packageType(*newest); t != "" {
			result["type"] = t
		}
		results = append(results, result)
	}
	sort.SliceStable(results, func(i, j int) bool {
		return rank(results[i], term) < rank(results[j], term)
	})

	total := len(results)
	from := (page - 1) * perPage
	if from > total {
		from = total
	}
	end := from + perPage
	if end > total {
		end = total
	}

	body, err := json.MarshalIndent(map[string]any{
		"results": results[from:end],
		"total":   total,
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

// hostedPackages reads the published facts, by package name.
func hostedPackages(ctx context.Context, feed api.Feed, deps api.CoreServices) (map[string][]hostedRelease, error) {
	manifests, err := deps.Manifests(ctx, feed, hostedStatePrefix)
	if err != nil {
		return nil, err
	}
	out := map[string][]hostedRelease{}
	for _, m := range manifests {
		body, err := readBlob(ctx, deps, m.SHA256)
		if err != nil {
			return nil, err
		}
		var release hostedRelease
		if err := json.Unmarshal(body, &release); err != nil {
			return nil, fmt.Errorf("parse package facts at %s: %w", m.Path, err)
		}
		out[release.Name] = append(out[release.Name], release)
	}
	return out, nil
}

func newestRelease(releases []hostedRelease) *hostedRelease {
	if len(releases) == 0 {
		return nil
	}
	sort.Slice(releases, func(i, j int) bool {
		return compareComposerVersions(releases[i].Version, releases[j].Version) < 0
	})
	return &releases[len(releases)-1]
}

func packageType(release hostedRelease) string {
	if release.Type != "" {
		return release.Type
	}
	return "library"
}

func matchesTerm(release hostedRelease, term string) bool {
	return strings.Contains(strings.ToLower(release.Name), term) ||
		strings.Contains(strings.ToLower(release.Description), term)
}

// rank orders results: an exact name, then a prefix, then everything else.
func rank(entry any, term string) int {
	result, _ := entry.(map[string]any)
	name, _ := result["name"].(string)
	name = strings.ToLower(name)
	switch {
	case term == "":
		return 1
	case name == term:
		return 0
	case strings.HasPrefix(name, term):
		return 1
	default:
		return 2
	}
}

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
