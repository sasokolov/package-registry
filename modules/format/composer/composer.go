// Package composer implements the FormatModule for the Composer v2
// repository protocol:
//
//	GET /packages.json                 root manifest (mutable)
//	GET /p2/{vendor}/{pkg}.json        package metadata (mutable)
//	GET /p2/{vendor}/{pkg}~dev.json    dev branches (mutable)
//	GET /dists/...                     distribution archives (immutable)
//
// RewriteMetadata points metadata-url/dist.url at this registry and keeps
// dist.shasum for verification.
package composer

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

// metadataTTL bounds package metadata freshness (SWR beyond it).
const metadataTTL = 5 * time.Minute

// distPrefix is the registry-side path prefix for rewritten dist URLs; the
// remainder is the upstream dist path, so Parse maps it straight back.
const distPrefix = "dists/"

func init() {
	api.RegisterFormat(Module{})
}

// Module implements api.FormatModule.
type Module struct{}

// Name implements api.FormatModule.
func (Module) Name() string { return "composer" }

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
	case p == "packages.json":
		return api.Intent{
			Kind:        api.IntentMetadata,
			Coord:       api.PackageCoordinate{Format: "composer", Name: "packages.json"},
			CacheTTL:    metadataTTL,
			RemotePath:  p,
			ContentType: "application/json",
		}, nil

	case strings.HasPrefix(p, "p2/") && strings.HasSuffix(p, ".json"):
		name := strings.TrimSuffix(strings.TrimPrefix(p, "p2/"), ".json")
		name = strings.TrimSuffix(name, "~dev")
		vendor, pkg, ok := strings.Cut(name, "/")
		if !ok || vendor == "" || pkg == "" || strings.Contains(pkg, "/") {
			return api.Intent{}, api.NotFoundf("not a composer package path: %q", p)
		}
		return api.Intent{
			Kind:        api.IntentMetadata,
			Coord:       api.PackageCoordinate{Format: "composer", Name: name},
			CacheTTL:    metadataTTL,
			RemotePath:  p,
			ContentType: "application/json",
		}, nil

	case strings.HasPrefix(p, distPrefix):
		rest := strings.TrimPrefix(p, distPrefix)
		encoded, file, ok := strings.Cut(rest, "/")
		if !ok || encoded == "" || file == "" || strings.Contains(file, "/") {
			return api.Intent{}, api.NotFoundf("not a composer dist path: %q", p)
		}
		distURL, err := decodeDistLocation(encoded)
		if err != nil {
			return api.Intent{}, api.NotFoundf("invalid dist location in %q", p)
		}
		return api.Intent{
			Kind:        api.IntentArtifact,
			Coord:       api.PackageCoordinate{Format: "composer", Name: distCoordinateFromFile(file)},
			RemotePath:  p,
			RemoteURL:   distURL,
			ContentType: "application/zip",
		}, nil

	default:
		return api.Intent{}, api.NotFoundf("unsupported composer path: %q", p)
	}
}

// Dist locations are opaque absolute URLs on arbitrary hosts (Packagist
// serves them from GitHub), so they are encoded into the registry path
// rather than mapped onto the feed's upstream.

func encodeDistLocation(distURL string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(distURL))
}

func decodeDistLocation(encoded string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(string(raw))
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("dist location %q is not an absolute http(s) URL", raw)
	}
	return u.String(), nil
}

// distCoordinateFromFile derives a readable coordinate from the dist file
// name ("<package>-<reference>.zip").
func distCoordinateFromFile(file string) string {
	name := strings.TrimSuffix(file, ".zip")
	if i := strings.LastIndex(name, "-"); i > 0 {
		name = name[:i]
	}
	return name
}

// RewriteMetadata implements api.FormatModule: repoint metadata-url and
// every dist.url at this registry while preserving dist.shasum and all
// other fields.
func (Module) RewriteMetadata(feed api.Feed, body []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse composer metadata: %w", err)
	}
	base := feedBase(feed)

	// Root manifest: point discovery at this registry.
	if _, ok := doc["packages"]; ok || doc["metadata-url"] != nil {
		if _, isRoot := doc["metadata-url"]; isRoot {
			doc["metadata-url"] = base + "/p2/%package%.json"
		}
		// A root manifest must not advertise upstream-only endpoints the
		// registry does not serve.
		delete(doc, "providers-url")
		delete(doc, "provider-includes")
		delete(doc, "search")
		delete(doc, "list")
	}

	if packages, ok := doc["packages"].(map[string]any); ok {
		for _, raw := range packages {
			versions, ok := raw.([]any)
			if !ok {
				continue
			}
			for _, item := range versions {
				version, ok := item.(map[string]any)
				if !ok {
					continue
				}
				dist, ok := version["dist"].(map[string]any)
				if !ok {
					continue
				}
				distURL, ok := dist["url"].(string)
				if !ok || distURL == "" {
					continue
				}
				rewritten, err := rewriteDistURL(base, distURL)
				if err != nil {
					return nil, err
				}
				dist["url"] = rewritten
			}
		}
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode composer metadata: %w", err)
	}
	return out, nil
}

func feedBase(feed api.Feed) string {
	prefix := "/composer/" + feed.Name
	if feed.ExternalURL != "" {
		return strings.TrimSuffix(feed.ExternalURL, "/") + prefix
	}
	return prefix
}

// rewriteDistURL maps an upstream dist URL onto this registry, keeping the
// original location inside the path so it survives caching and reloads.
func rewriteDistURL(base, distURL string) (string, error) {
	u, err := url.Parse(distURL)
	if err != nil || !u.IsAbs() {
		return "", fmt.Errorf("dist url %q is not absolute: %w", distURL, err)
	}
	file := path.Base(u.EscapedPath())
	if file == "" || file == "." || file == "/" {
		return "", fmt.Errorf("dist url %q has no file name", distURL)
	}
	if !strings.HasSuffix(file, ".zip") {
		file += ".zip"
	}
	return base + "/" + distPrefix + encodeDistLocation(distURL) + "/" + file, nil
}

// MetadataIntent points at the package's p2 document, which carries the
// license, publication time and dist digest.
func (Module) MetadataIntent(_ api.Feed, coord api.PackageCoordinate) (api.Intent, bool) {
	if coord.Name == "" || !strings.Contains(coord.Name, "/") {
		return api.Intent{}, false
	}
	return api.Intent{
		Kind:        api.IntentMetadata,
		Coord:       api.PackageCoordinate{Format: "composer", Name: coord.Name},
		CacheTTL:    metadataTTL,
		RemotePath:  "p2/" + coord.Name + ".json",
		ContentType: "application/json",
	}, true
}

// packageDoc is the subset of a p2 document we interpret.
type packageDoc struct {
	Packages map[string][]struct {
		Version string   `json:"version"`
		License []string `json:"license"`
		Time    string   `json:"time"`
		Dist    struct {
			Shasum string `json:"shasum"`
		} `json:"dist"`
	} `json:"packages"`
}

// ExtractMetadata pulls canonical keys for a coordinate out of a p2
// document. Composer dist URLs carry a reference rather than a version, so
// the checksum is only reported when the version is known.
func (Module) ExtractMetadata(coord api.PackageCoordinate, body []byte) (map[string]string, error) {
	var doc packageDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse composer package document: %w", err)
	}
	meta := map[string]string{api.MetaEcosystem: "Packagist"}
	versions, ok := doc.Packages[coord.Name]
	if !ok || len(versions) == 0 {
		return meta, nil
	}
	for _, v := range versions {
		if coord.Version != "" && v.Version != coord.Version {
			continue
		}
		if len(v.License) > 0 {
			meta[api.MetaLicense] = strings.Join(v.License, " OR ")
		}
		if v.Time != "" {
			meta[api.MetaPublishedAt] = v.Time
		}
		if v.Dist.Shasum != "" && coord.Version != "" {
			meta[api.MetaChecksum] = "sha1:" + strings.ToLower(v.Dist.Shasum)
		}
		break
	}
	return meta, nil
}

// ValidateFeeds implements api.FeedSetValidator: composer resolves dist and
// metadata URLs literally, so a relative rewrite base silently breaks
// installs. Require site.external_url whenever a composer feed exists.
func (Module) ValidateFeeds(feeds []api.Feed) error {
	for _, f := range feeds {
		if f.ExternalURL == "" {
			return fmt.Errorf("feed %s: composer needs site.external_url — composer does not resolve relative dist/metadata URLs", f.Name)
		}
	}
	return nil
}
