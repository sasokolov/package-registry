package helm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/modules/internal/semver"
)

// ChartMuseum's own API: what does this repository hold.
//
//	GET /api/charts             every chart, newest version first
//	GET /api/charts/{name}      one chart's versions
//	GET /api/charts/{name}/{v}  one version
//
// It is answered from the published manifests per request rather than
// generated into an index, because it is the same information index.yaml
// already carries: a second stored copy would be a second thing to keep in
// step, and the only way for a registry to contradict itself.

// Search implements api.Searcher.
func (Module) Search(ctx context.Context, feed api.Feed, intent api.Intent, deps api.CoreServices) (api.SyntheticResponse, error) {
	charts, err := hostedCharts(ctx, feed, deps)
	if err != nil {
		return api.SyntheticResponse{}, err
	}

	name, version := "", ""
	if rest := strings.Trim(intent.Coord.Name, "/"); rest != "" {
		name, version, _ = strings.Cut(rest, "/")
	}

	switch {
	case name == "":
		byName := map[string][]map[string]any{}
		for _, chart := range charts {
			byName[chart.Name] = append(byName[chart.Name], listingEntry(chart))
		}
		for n := range byName {
			sortEntries(byName[n])
		}
		return jsonResponse(http.StatusOK, byName)

	case version == "":
		entries := []map[string]any{}
		for _, chart := range charts {
			if chart.Name == name {
				entries = append(entries, listingEntry(chart))
			}
		}
		if len(entries) == 0 {
			return notFound(name)
		}
		sortEntries(entries)
		return jsonResponse(http.StatusOK, entries)

	default:
		for _, chart := range charts {
			if chart.Name == name && chart.Version == version {
				return jsonResponse(http.StatusOK, listingEntry(chart))
			}
		}
		return notFound(name + "-" + version)
	}
}

// hostedCharts reads what the feed holds, archives joined to the metadata
// stored beside them.
func hostedCharts(ctx context.Context, feed api.Feed, deps api.CoreServices) ([]storedChart, error) {
	manifests, err := deps.Manifests(ctx, feed, "")
	if err != nil {
		return nil, fmt.Errorf("list helm manifests: %w", err)
	}
	metadata := map[string][]byte{}
	var out []storedChart
	for _, m := range manifests {
		if strings.HasPrefix(m.Path, hostedPrefix) {
			body, err := readBlob(ctx, deps, m.SHA256)
			if err != nil {
				return nil, err
			}
			metadata[m.Coord.Name+"@"+m.Coord.Version] = body
		}
	}
	for _, m := range manifests {
		if !strings.HasPrefix(m.Path, chartsPrefix) || strings.HasSuffix(m.Path, ".prov") {
			continue
		}
		chart := storedChart{
			Name: m.Coord.Name, Version: m.Coord.Version, Digest: m.SHA256,
			Created: m.PublishedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if raw, ok := metadata[m.Coord.Name+"@"+m.Coord.Version]; ok {
			var doc map[string]any
			if err := yaml.Unmarshal(raw, &doc); err == nil {
				chart.Metadata = doc
			}
		}
		out = append(out, chart)
	}
	return out, nil
}

func listingEntry(chart storedChart) map[string]any {
	entry := map[string]any{}
	for k, v := range chart.Metadata {
		entry[k] = v
	}
	entry["name"] = chart.Name
	entry["version"] = chart.Version
	entry["digest"] = chart.Digest
	if chart.Created != "" {
		entry["created"] = chart.Created
	}
	return entry
}

func sortEntries(entries []map[string]any) {
	sort.SliceStable(entries, func(i, j int) bool {
		return semver.Compare(str(entries[i]["version"]), str(entries[j]["version"])) > 0
	})
}

func jsonResponse(status int, body any) (api.SyntheticResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return api.SyntheticResponse{}, fmt.Errorf("encode chart listing: %w", err)
	}
	return api.SyntheticResponse{
		Status: status,
		Header: map[string]string{"Content-Type": "application/json"},
		Body:   raw,
	}, nil
}

func notFound(what string) (api.SyntheticResponse, error) {
	return jsonResponse(http.StatusNotFound, map[string]string{"error": "no chart " + what})
}
