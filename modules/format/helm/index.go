package helm

import (
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/modules/internal/semver"
)

// The repository index.
//
// It is the only document Helm reads to learn what a repository has, and
// every entry carries the URL its archive is at. Proxying a repository is
// therefore mostly a matter of rewriting those URLs to point here — a client
// that got the upstream's URLs would download straight from the upstream and
// the cache would never fill.
//
// Fields are kept as a map rather than a struct on purpose: an index entry
// carries whatever the chart's metadata had (annotations, dependencies,
// kubeVersion, keywords, and whatever Helm adds next), and a struct would
// quietly drop the parts this module has not heard of.

// chartIndex is the top level of index.yaml.
type chartIndex struct {
	APIVersion string                      `yaml:"apiVersion"`
	Entries    map[string][]map[string]any `yaml:"entries"`
	Generated  string                      `yaml:"generated,omitempty"`
	// Rest keeps anything else the document had, so a round trip through
	// this module is not a way to lose information.
	Rest map[string]any `yaml:",inline"`
}

// RewriteMetadata implements api.FormatModule: point every chart URL at this
// feed.
func (Module) RewriteMetadata(feed api.Feed, body []byte) ([]byte, error) {
	var index chartIndex
	if err := yaml.Unmarshal(body, &index); err != nil {
		return nil, fmt.Errorf("parse helm index: %w", err)
	}
	if index.Entries == nil {
		// Not an index — a repository that answers index.yaml with
		// something else is not one this module can serve, and passing the
		// body through unchanged would hand the client the upstream's URLs.
		return nil, fmt.Errorf("helm index has no entries")
	}

	for _, versions := range index.Entries {
		for _, entry := range versions {
			raw, ok := entry["urls"].([]any)
			if !ok {
				continue
			}
			rewritten := make([]any, 0, len(raw))
			for _, u := range raw {
				location, ok := u.(string)
				if !ok {
					rewritten = append(rewritten, u)
					continue
				}
				rewritten = append(rewritten, chartURL(feed, feed.Upstream, location))
			}
			entry["urls"] = rewritten
		}
	}

	out, err := yaml.Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("encode helm index: %w", err)
	}
	return out, nil
}

// buildIndex assembles an index.yaml from what a feed hosts.
//
// It is a pure function of the manifest set, like every other generated
// index here: that is what lets a site rebuild it locally instead of
// replicating it (invariant 15), and what makes two sites agree without
// coordinating.
func buildIndex(feed api.Feed, charts []storedChart, now time.Time) ([]byte, error) {
	index := chartIndex{
		APIVersion: "v1",
		Entries:    map[string][]map[string]any{},
		Generated:  now.UTC().Format(time.RFC3339),
	}

	base := feedBase(feed)
	for _, chart := range charts {
		entry := map[string]any{}
		for k, v := range chart.Metadata {
			entry[k] = v
		}
		entry["name"] = chart.Name
		entry["version"] = chart.Version
		entry["urls"] = []any{base + "/" + chartsPrefix + chartFile(chart.Name, chart.Version)}
		entry["digest"] = chart.Digest
		if chart.Created != "" {
			entry["created"] = chart.Created
		}
		index.Entries[chart.Name] = append(index.Entries[chart.Name], entry)
	}

	// Newest first within a chart, and a stable order between runs: an index
	// that shuffles on every rebuild is a diff nobody can read and a cache
	// that revalidates for nothing.
	for name := range index.Entries {
		versions := index.Entries[name]
		sort.SliceStable(versions, func(i, j int) bool {
			return semver.Compare(str(versions[i]["version"]), str(versions[j]["version"])) > 0
		})
	}

	out, err := yaml.Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("encode helm index: %w", err)
	}
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
