// Package helm implements the FormatModule for Helm chart repositories, in
// the shape ChartMuseum made the de-facto standard:
//
//	GET  /index.yaml                    repository index (mutable)
//	GET  /charts/{name}-{version}.tgz   chart archive (immutable artifact)
//	GET  /charts/{name}-{version}.tgz.prov  provenance signature
//	POST /api/charts                    upload a chart
//	GET  /api/charts                    what this feed holds
//	GET  /api/charts/{name}             the versions of one chart
//
// A Helm repository is one index document plus a pile of tarballs, which is
// the same shape as Maven's maven-metadata.xml and its jars: the index is
// mutable and cached with a TTL, the archives are immutable and cached
// forever.
//
// Two things about it are worth knowing before reading the code.
//
// The file name is ambiguous by construction. Both a chart's name and its
// version may contain "-", so "nginx-ingress-4.1.0.tgz" could be split three
// ways and nothing in the file name says which is right. On publish the
// answer is exact — Chart.yaml inside the archive says — and that is what
// this module uses. On a proxied download there is no archive to read yet,
// so the split is a heuristic and the index remains the authority on what
// exists.
//
// ChartMuseum allows overwriting and deleting a published version. This
// registry does not (invariant 4): a version that somebody may have deployed
// does not quietly become different bytes. Taking a chart out of circulation
// is quarantine, which is reversible and leaves a record.
package helm

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
)

// indexTTL bounds index freshness; beyond it the pipeline revalidates and,
// if the upstream is down, serves stale (invariant 6).
const indexTTL = 5 * time.Minute

// Paths of the protocol.
const (
	indexPath    = "index.yaml"
	chartsPrefix = "charts/"
	apiPrefix    = "api/charts"
	// remotePrefix is where a chart URL that does not live under the feed's
	// upstream is served from. A repository index may point anywhere — a
	// release asset on GitHub, a CDN — and those cannot be mapped onto the
	// upstream base, so the location travels in the path.
	remotePrefix = "remote/"
)

func init() {
	api.RegisterFormat(Module{})
}

// Module implements api.FormatModule.
type Module struct{}

// Name implements api.FormatModule.
func (Module) Name() string { return "helm" }

// Routes implements api.FormatModule.
func (Module) Routes() []api.Route {
	return []api.Route{
		{Method: http.MethodGet, Pattern: "/*"},
		{Method: http.MethodHead, Pattern: "/*"},
	}
}

// Parse implements api.FormatModule.
func (Module) Parse(r *http.Request) (api.Intent, error) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" || strings.Contains(p, "..") {
		return api.Intent{}, api.NotFoundf("invalid path %q", p)
	}

	switch {
	case p == indexPath:
		return api.Intent{
			Kind:        api.IntentMetadata,
			Coord:       api.PackageCoordinate{Format: "helm", Name: indexPath},
			CacheTTL:    indexTTL,
			RemotePath:  indexPath,
			ContentType: "application/x-yaml",
		}, nil

	case p == apiPrefix || strings.HasPrefix(p, apiPrefix+"/"):
		// "What does this feed hold" is answered per request from the
		// published manifests, like any other search: an index of it would
		// be a second thing to keep in step with the first.
		//
		// The coordinate carries what was asked about — nothing for the
		// whole feed, a chart name, or a name and version — so the answer
		// is a function of the request and not of the path's spelling.
		var what string
		if p != apiPrefix {
			what = strings.TrimPrefix(p, apiPrefix+"/")
		}
		return api.Intent{
			Kind:        api.IntentSearch,
			Coord:       api.PackageCoordinate{Format: "helm", Name: what},
			RemotePath:  p,
			ContentType: "application/json",
		}, nil

	case strings.HasPrefix(p, chartsPrefix):
		return chartIntent(p, strings.TrimPrefix(p, chartsPrefix), "")

	case strings.HasPrefix(p, remotePrefix):
		rest := strings.TrimPrefix(p, remotePrefix)
		encoded, file, ok := strings.Cut(rest, "/")
		if !ok || encoded == "" || file == "" || strings.Contains(file, "/") {
			return api.Intent{}, api.NotFoundf("not a chart location path: %q", p)
		}
		location, err := decodeLocation(encoded)
		if err != nil {
			return api.Intent{}, api.NotFoundf("invalid chart location in %q", p)
		}
		return chartIntent(p, file, location)

	default:
		return api.Intent{}, api.NotFoundf("unsupported helm path: %q", p)
	}
}

// chartIntent describes one archive. remoteURL is empty when the archive
// lives under the feed's own upstream.
func chartIntent(requestPath, file, remoteURL string) (api.Intent, error) {
	if file == "" || strings.Contains(file, "/") {
		return api.Intent{}, api.NotFoundf("not a chart file: %q", requestPath)
	}
	name, version := splitChartFile(file)
	if name == "" {
		return api.Intent{}, api.NotFoundf("not a chart file name: %q", file)
	}
	contentType := "application/gzip"
	if strings.HasSuffix(file, ".prov") {
		// A provenance file is a detached signature, and Helm reads it as
		// text.
		contentType = "text/plain; charset=utf-8"
	}
	return api.Intent{
		Kind:        api.IntentArtifact,
		Coord:       api.PackageCoordinate{Format: "helm", Name: name, Version: version},
		RemotePath:  requestPath,
		RemoteURL:   remoteURL,
		ContentType: contentType,
	}, nil
}

// splitChartFile takes "<name>-<version>.tgz" apart.
//
// Both halves may contain "-", so this is a guess, and it is the same guess
// Helm itself makes: the version is what follows the first "-" that is
// followed by a digit. "nginx-ingress-4.1.0.tgz" splits after "ingress"
// because "ingress" does not start with a digit and "4.1.0" does.
//
// A published chart never relies on this — Chart.yaml is read from the
// archive — and a proxied one is listed by the upstream's index, which
// carries the name and version explicitly. This only has to produce a stable
// coordinate for the access rules and the audit log.
func splitChartFile(file string) (name, version string) {
	base := strings.TrimSuffix(strings.TrimSuffix(file, ".prov"), ".tgz")
	if base == "" {
		return "", ""
	}
	for i := 0; i < len(base); i++ {
		if base[i] != '-' || i+1 >= len(base) {
			continue
		}
		if next := base[i+1]; next >= '0' && next <= '9' {
			return base[:i], base[i+1:]
		}
	}
	// No version in the name at all: still a chart, just an unversioned
	// coordinate rather than a wrong one.
	return base, ""
}

// chartFile is the archive name Helm expects for a coordinate.
func chartFile(name, version string) string {
	if version == "" {
		return name + ".tgz"
	}
	return name + "-" + version + ".tgz"
}

// RedirectSafeIntent implements api.RedirectSafe: Helm verifies a chart
// against the digest in the index, so an archive may be answered with a
// pre-signed redirect. The index itself never can — it is what carries the
// digests, and it is rewritten for this feed.
func (Module) RedirectSafeIntent(intent api.Intent) bool {
	return intent.Kind == api.IntentArtifact
}

// Chart locations are opaque absolute URLs on arbitrary hosts, so they are
// encoded into the registry path rather than mapped onto the upstream base.

func encodeLocation(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeLocation(encoded string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(string(raw))
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("chart location %q is not an absolute http(s) URL", raw)
	}
	return u.String(), nil
}

// feedBase is the absolute URL clients reach this feed at, which is what the
// rewritten index has to point at.
func feedBase(feed api.Feed) string {
	external := strings.TrimSuffix(feed.ExternalURL, "/")
	return external + "/helm/" + feed.Name
}

// chartURL is where this registry serves an upstream chart URL from.
//
// A URL under the feed's own upstream keeps its path, so the cache key is
// the protocol path and looks like what a person would expect. Anything else
// travels encoded: a repository index is free to point at a CDN, and the
// alternative to carrying that location is refusing to proxy the chart.
func chartURL(feed api.Feed, upstream, raw string) string {
	base := feedBase(feed)
	trimmedUpstream := strings.TrimSuffix(upstream, "/")

	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return raw
	case !u.IsAbs():
		// Relative to the index, which is the common case: charts sit next
		// to it or in charts/ beside it.
		return base + "/" + chartsPrefix + path.Base(u.Path)
	case trimmedUpstream != "" && strings.HasPrefix(raw, trimmedUpstream+"/"):
		return base + "/" + strings.TrimPrefix(raw, trimmedUpstream+"/")
	default:
		file := path.Base(u.EscapedPath())
		if file == "" || file == "." || file == "/" {
			return raw
		}
		return base + "/" + remotePrefix + encodeLocation(raw) + "/" + file
	}
}
