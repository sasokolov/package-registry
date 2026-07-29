package npm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/modules/internal/semver"
)

// Search over a hosted feed.
//
// It is answered from the same per-version documents the package roots are
// built from, so a search can never show something a subsequent install
// would fail to find.
//
// The scores are the honest part. npm's search returns quality, popularity
// and maintenance figures computed from download counts and repository
// activity; a private registry measures none of that. Rather than invent
// numbers, every result reports the same score and the ordering carries the
// meaning: exact name first, then a prefix, then anything matching.

// searchDefaultSize is what `npm search` asks for when it does not say.
const searchDefaultSize = 20

// searchMaxSize bounds a page.
const searchMaxSize = 250

// Search implements api.Searcher.
func (Module) Search(ctx context.Context, feed api.Feed, intent api.Intent, deps api.CoreServices) (api.SyntheticResponse, error) {
	params, err := url.ParseQuery(intent.RemoteQuery)
	if err != nil {
		return api.SyntheticResponse{}, fmt.Errorf("invalid search query: %v: %w", err, api.ErrBadRequest)
	}
	term := strings.ToLower(strings.TrimSpace(params.Get("text")))
	from := intParam(params.Get("from"), 0)
	size := intParam(params.Get("size"), searchDefaultSize)
	if size > searchMaxSize {
		size = searchMaxSize
	}

	packages, err := hostedPackages(ctx, feed, deps)
	if err != nil {
		return api.SyntheticResponse{}, err
	}

	names := make([]string, 0, len(packages))
	for name := range packages {
		names = append(names, name)
	}
	sort.Strings(names)

	base := feedBase(feed)
	objects := make([]any, 0, len(names))
	for _, name := range names {
		entry, ok := searchObject(base, name, packages[name])
		if !ok {
			continue
		}
		if term != "" && !matchesTerm(entry, term) {
			continue
		}
		objects = append(objects, entry)
	}
	sort.SliceStable(objects, func(i, j int) bool {
		return rank(objects[i], term) < rank(objects[j], term)
	})

	total := len(objects)
	if from > total {
		from = total
	}
	end := from + size
	if end > total {
		end = total
	}

	body, err := json.MarshalIndent(map[string]any{
		"objects": objects[from:end],
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

// hostedVersion is one published version as search sees it.
type hostedVersion struct {
	doc         json.RawMessage
	publishedAt time.Time
}

// hostedPackages reads the published per-version documents, by package name.
func hostedPackages(ctx context.Context, feed api.Feed, deps api.CoreServices) (map[string]map[string]hostedVersion, error) {
	manifests, err := deps.Manifests(ctx, feed, hostedPrefix)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]hostedVersion{}
	for _, m := range manifests {
		name, version, ok := versionDocOf(m.Path)
		if !ok {
			continue
		}
		body, err := readBlob(ctx, deps, m.SHA256)
		if err != nil {
			return nil, err
		}
		if out[name] == nil {
			out[name] = map[string]hostedVersion{}
		}
		out[name][version] = hostedVersion{doc: body, publishedAt: m.PublishedAt}
	}
	return out, nil
}

// versionDocOf recognises -/hosted/{pkg}/versions/{version}.json, which is
// where a publish records what it published.
func versionDocOf(path string) (name, version string, ok bool) {
	rest, found := strings.CutPrefix(path, hostedPrefix)
	if !found {
		return "", "", false
	}
	idx := strings.Index(rest, "/versions/")
	if idx <= 0 {
		return "", "", false
	}
	name = rest[:idx]
	version = strings.TrimSuffix(rest[idx+len("/versions/"):], ".json")
	if name == "" || version == "" {
		return "", "", false
	}
	return name, version, true
}

// searchObject renders one package: its newest version, described the way
// `npm search` expects.
//
// The optional-looking fields are not optional. npm's own formatter maps
// over keywords and maintainers and reads links and date without checking
// whether they are there, so a document that merely omits what it does not
// know crashes the client rather than showing fewer columns. Empty is the
// honest value; absent is a bug report.
func searchObject(base, name string, versions map[string]hostedVersion) (map[string]any, bool) {
	list := make([]string, 0, len(versions))
	for v := range versions {
		list = append(list, v)
	}
	if len(list) == 0 {
		return nil, false
	}
	sort.Slice(list, func(i, j int) bool { return semver.Compare(list[i], list[j]) < 0 })
	newest := list[len(list)-1]

	var doc map[string]any
	if err := json.Unmarshal(versions[newest].doc, &doc); err != nil {
		// A version document we cannot read is not a reason to fail the
		// whole search; it is one package that will not appear.
		return nil, false
	}

	pkg := map[string]any{
		"name":        name,
		"version":     newest,
		"description": "",
		"keywords":    []any{},
		"maintainers": []any{},
		"date":        versions[newest].publishedAt.UTC().Format(time.RFC3339),
		"links":       map[string]any{"npm": base + "/" + name},
	}
	for _, key := range []string{"description", "keywords", "author", "license", "homepage"} {
		if value, ok := doc[key]; ok && value != nil {
			pkg[key] = value
		}
	}

	return map[string]any{
		"package": pkg,
		// One score for everything: a private registry measures neither
		// downloads nor repository activity, and a fabricated ranking is
		// worse than an admitted absence of one.
		"score": map[string]any{
			"final":  0,
			"detail": map[string]any{"quality": 0, "popularity": 0, "maintenance": 0},
		},
		"searchScore": 0,
	}, true
}

func matchesTerm(entry map[string]any, term string) bool {
	pkg, _ := entry["package"].(map[string]any)
	if pkg == nil {
		return false
	}
	for _, key := range []string{"name", "description"} {
		if value, ok := pkg[key].(string); ok && strings.Contains(strings.ToLower(value), term) {
			return true
		}
	}
	if keywords, ok := pkg["keywords"].([]any); ok {
		for _, k := range keywords {
			if value, ok := k.(string); ok && strings.Contains(strings.ToLower(value), term) {
				return true
			}
		}
	}
	return false
}

// rank orders results: an exact name, then a prefix, then everything else.
func rank(entry any, term string) int {
	object, _ := entry.(map[string]any)
	pkg, _ := object["package"].(map[string]any)
	name, _ := pkg["name"].(string)
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
