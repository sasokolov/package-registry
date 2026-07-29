package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/fondaco-dev/fondaco/core/api"
)

// What a feed holds: the two listing endpoints of the protocol.
//
//	GET /v2/{name}/tags/list   the tags of one repository
//	GET /v2/_catalog           the repositories of this feed
//
// Both are answered from the published coordinates per request rather than
// from a generated document, because they are the same information the
// coordinates already carry: a stored copy would be a second thing to keep
// in step, and the only way for a registry to contradict itself.
//
// A proxy does not come here at all — its upstream answers, and the answer
// is cached like any other mutable document.

// defaultPageSize is how many entries an unpaginated request gets. The spec
// leaves it to the registry; this is what the reference implementation uses.
const defaultPageSize = 100

// Search implements api.Searcher.
func (Module) Search(ctx context.Context, feed api.Feed, intent api.Intent,
	deps api.CoreServices) (api.SyntheticResponse, error) {
	page := parsePage(intent.RemoteQuery)

	if strings.HasSuffix(intent.RemotePath, "/_catalog") {
		repos, err := hostedRepositories(ctx, feed, deps)
		if err != nil {
			return api.SyntheticResponse{}, err
		}
		shown, truncated := page.apply(repos)
		return listing(map[string]any{"repositories": shown}, page, shown, truncated)
	}

	repo := intent.Coord.Name
	tags, err := hostedTags(ctx, feed, deps, repo)
	if err != nil {
		return api.SyntheticResponse{}, err
	}
	if len(tags) == 0 {
		// The spec's own answer for a repository that is not here. It has to
		// be a status rather than an empty list: a client reading an empty
		// list would conclude the image was deleted.
		return errorResponse(http.StatusNotFound, "NAME_UNKNOWN",
			"repository "+repo+" is not in this feed")
	}
	shown, truncated := page.apply(tags)
	return listing(map[string]any{"name": repo, "tags": shown}, page, shown, truncated)
}

// hostedTags lists the tags of one repository, newest first is NOT the order
// here: the protocol says lexical, and clients paginate on it.
func hostedTags(ctx context.Context, feed api.Feed, deps api.CoreServices, repo string) ([]string, error) {
	prefix := apiRoot + "/" + repo + sepManifests
	manifests, err := deps.Manifests(ctx, feed, prefix)
	if err != nil {
		return nil, fmt.Errorf("list manifests of %s: %w", repo, err)
	}
	var tags []string
	for _, m := range manifests {
		reference := strings.TrimPrefix(m.Path, prefix)
		if reference == "" || strings.Contains(reference, "/") {
			continue
		}
		if _, isDigest := parseDigest(reference); isDigest {
			continue
		}
		tags = append(tags, reference)
	}
	sort.Strings(tags)
	return tags, nil
}

// hostedRepositories lists every repository this feed has a manifest for.
func hostedRepositories(ctx context.Context, feed api.Feed, deps api.CoreServices) ([]string, error) {
	manifests, err := deps.Manifests(ctx, feed, apiRoot+"/")
	if err != nil {
		return nil, fmt.Errorf("list manifests: %w", err)
	}
	seen := map[string]bool{}
	var repos []string
	for _, m := range manifests {
		i := strings.LastIndex(m.Path, sepManifests)
		if i < 0 {
			continue // a blob: its repository is named by a manifest anyway
		}
		repo := strings.TrimPrefix(m.Path[:i], apiRoot+"/")
		if repo == "" || seen[repo] {
			continue
		}
		seen[repo] = true
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos, nil
}

// pageRequest is the protocol's pagination: how many, and after which entry.
type pageRequest struct {
	n    int
	last string
}

func parsePage(rawQuery string) pageRequest {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return pageRequest{n: defaultPageSize}
	}
	page := pageRequest{n: defaultPageSize, last: values.Get("last")}
	if raw := values.Get("n"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			page.n = n
		}
	}
	return page
}

// apply cuts the window the client asked for out of a sorted list.
func (p pageRequest) apply(all []string) (shown []string, truncated bool) {
	start := 0
	if p.last != "" {
		start = sort.SearchStrings(all, p.last)
		if start < len(all) && all[start] == p.last {
			start++
		}
	}
	rest := all[start:]
	if p.n > 0 && len(rest) > p.n {
		return rest[:p.n], true
	}
	if rest == nil {
		rest = []string{}
	}
	return rest, false
}

// listing renders one page, with the Link header that says there is more.
func listing(body map[string]any, page pageRequest, shown []string, truncated bool) (api.SyntheticResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return api.SyntheticResponse{}, fmt.Errorf("encode listing: %w", err)
	}
	header := map[string]string{"Content-Type": mediaTypeJSON}
	if truncated && len(shown) > 0 {
		query := url.Values{}
		query.Set("n", strconv.Itoa(page.n))
		query.Set("last", shown[len(shown)-1])
		header["Link"] = `<?` + query.Encode() + `>; rel="next"`
	}
	return api.SyntheticResponse{Status: http.StatusOK, Header: header, Body: raw}, nil
}

// errorResponse is the shape this protocol's clients parse a failure from.
func errorResponse(status int, code, message string) (api.SyntheticResponse, error) {
	raw, err := json.Marshal(map[string]any{
		"errors": []map[string]string{{"code": code, "message": message}},
	})
	if err != nil {
		return api.SyntheticResponse{}, err
	}
	return api.SyntheticResponse{
		Status: status,
		Header: map[string]string{"Content-Type": mediaTypeJSON},
		Body:   raw,
	}, nil
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
