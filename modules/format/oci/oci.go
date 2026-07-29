// Package oci implements the FormatModule for OCI Distribution — the API
// docker, podman, containerd, buildah, crane and helm's OCI mode all speak:
//
//	GET  /v2/                                 API probe
//	GET  /v2/{name}/manifests/{tag|digest}    an image or an index
//	GET  /v2/{name}/blobs/{digest}            a layer or a config
//	GET  /v2/{name}/tags/list                 what tags this repository has
//	GET  /v2/_catalog                         what repositories this feed has
//	POST /v2/{name}/blobs/uploads/            start an upload
//	PUT  /v2/{name}/manifests/{reference}     publish an image
//
// Three things about it are different from every other format here.
//
// The protocol owns the whole URL. A client builds every request path from
// the image reference and always addresses /v2/ at the host root, so this
// registry's mount point has to be part of the image NAME:
//
//	docker pull registry.example/oci/hub/library/alpine:3.20
//	              ->  GET /v2/oci/hub/library/alpine/manifests/3.20
//
// which is the same /{format}/{feed} prefix every other feed is reached by,
// only inside the path the protocol dictates rather than in front of it
// (api.FeedRouter).
//
// An image is not one file. It is a manifest naming a config blob and some
// layer blobs, or an index naming manifests per platform. Every one of them
// is addressed by its own digest, which makes the whole tree content
// addressed already — the digest is in the URL, so an artifact is verified
// against it for free (invariant 5) and a layer shared by ten images is one
// blob (invariant 10). This is the only protocol here that needs no
// checksum discovery at all.
//
// A tag is a pointer, and pointers move. The immutable thing an OCI client
// deploys is the digest; `latest` moving to a new image is the protocol
// working as designed, so a tag is published as a mutable coordinate while
// the manifest it points at is immutable like any other release (invariant
// 4). Overwriting a manifest at a digest is impossible by construction, and
// deleting one is refused: taking an image out of circulation is quarantine.
package oci

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
)

// formatName is both the module name and the first path segment of an image
// name served by this registry.
const formatName = "oci"

// apiRoot is where the protocol says a registry lives. It is not
// configurable: clients derive it from the image reference.
const apiRoot = "v2"

// tagTTL bounds how long a proxied tag is served without asking the upstream
// again. Tags move — that is what they are for — so this is the window in
// which a client can be told about an image that has since been retagged.
// Digests are immutable and are cached forever.
const tagTTL = 2 * time.Minute

// manifestAccept is what a manifest request asks for. Content negotiation is
// part of this protocol rather than an optimization: a registry answers the
// same URL with an image manifest, an index, or a deprecated schema
// depending on this header, so asking without it gets the wrong document.
// Schema 1 is deliberately absent — it is deprecated, unverifiable, and
// nothing this registry serves should be it.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Media types this registry accepts on publish and stores as-is.
const (
	mediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaTypeJSON           = "application/json"
	mediaTypeOctetStream    = "application/octet-stream"
)

func init() {
	api.RegisterFormat(Module{})
}

// Module implements api.FormatModule.
type Module struct{}

// Name implements api.FormatModule.
func (Module) Name() string { return formatName }

// Routes implements api.FormatModule: the feed's own mount point serves the
// same paths, so /oci/{feed}/v2/... works for anything that addresses the
// registry directly rather than as a container registry.
func (Module) Routes() []api.Route {
	return []api.Route{
		{Method: http.MethodGet, Pattern: "/*"},
		{Method: http.MethodHead, Pattern: "/*"},
	}
}

// FeedRoutes implements api.FeedRouter: the paths the protocol dictates,
// which live at the site root because a client cannot be told otherwise.
func (Module) FeedRoutes() []api.Route {
	return []api.Route{
		{Method: http.MethodGet, Pattern: "/" + apiRoot + "/*"},
		{Method: http.MethodHead, Pattern: "/" + apiRoot + "/*"},
		{Method: http.MethodPost, Pattern: "/" + apiRoot + "/*"},
		{Method: http.MethodPatch, Pattern: "/" + apiRoot + "/*"},
		{Method: http.MethodPut, Pattern: "/" + apiRoot + "/*"},
		{Method: http.MethodDelete, Pattern: "/" + apiRoot + "/*"},
	}
}

// RouteToFeed implements api.FeedRouter: /v2/{format}/{feed}/{name}/... is
// the image name a client was given, and the first two segments of it are
// where this registry keeps the feed.
func (Module) RouteToFeed(path string) (feed, feedPath string, ok bool) {
	rest, ok := strings.CutPrefix(path, "/"+apiRoot+"/")
	if !ok {
		return "", "", false
	}
	format, rest, ok := strings.Cut(rest, "/")
	if !ok || format != formatName {
		return "", "", false
	}
	feed, rest, ok = strings.Cut(rest, "/")
	if !ok || feed == "" || strings.Contains(feed, "..") {
		return "", "", false
	}
	return feed, "/" + apiRoot + "/" + rest, true
}

// RootRoutes implements api.RootRouter: the API probe, which names no feed.
func (Module) RootRoutes() []api.Route {
	return []api.Route{
		{Method: http.MethodGet, Pattern: "/" + apiRoot + "/"},
		{Method: http.MethodHead, Pattern: "/" + apiRoot + "/"},
	}
}

// ServeRoot answers the probe every client sends before anything else. It
// says "this is a v2 registry" and nothing about who is asking: whether a
// particular repository may be read is decided per request, on the feed that
// holds it, and answering the probe with a 401 would make an anonymous pull
// from a public feed impossible.
func (Module) ServeRoot(w http.ResponseWriter, r *http.Request, _ []api.Feed) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	w.Header().Set("Content-Type", mediaTypeJSON)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("{}"))
	}
}

// AuthChallenge implements api.AuthChallenger.
//
// Only Basic. The registry's default offers Bearer as well, which is right
// for tools that read the header as a list of options — but a docker client
// takes the first scheme it has a handler for, and offered Bearer it goes
// looking for a token service that does not exist here. `docker login`
// sends the token as the password, which is the same credential verified the
// same way.
func (Module) AuthChallenge(api.Feed) string { return `Basic realm="fondaco"` }

// RedirectSafeIntent implements api.RedirectSafe: a client verifies every
// blob against the digest it asked for, so a layer may be answered with a
// pre-signed redirect. A manifest never is — it is what carries the digests.
func (Module) RedirectSafeIntent(intent api.Intent) bool {
	return intent.Kind == api.IntentArtifact && !isManifestPath(intent.RemotePath)
}

// ResponseHeaders implements api.ResponseHeaderer.
//
// Docker-Content-Digest is how a client learns what it just fetched: pulling
// by tag, it is the only place the image's identity appears, and containerd
// uses it as the key everything else is stored under.
func (Module) ResponseHeaders(_ api.Feed, intent api.Intent, sha256hex string) map[string]string {
	headers := map[string]string{"Docker-Distribution-API-Version": "registry/2.0"}
	if sha256hex != "" && (isManifestPath(intent.RemotePath) || isBlobPath(intent.RemotePath)) {
		headers["Docker-Content-Digest"] = "sha256:" + sha256hex
	}
	return headers
}

// Parse implements api.FormatModule.
func (Module) Parse(r *http.Request) (api.Intent, error) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if strings.Contains(p, "..") {
		return api.Intent{}, api.NotFoundf("invalid path %q", p)
	}
	rest, ok := strings.CutPrefix(p, apiRoot+"/")
	if !ok {
		return api.Intent{}, api.NotFoundf("not a registry API path: %q", p)
	}

	// The catalog is the one endpoint with no repository in it.
	if rest == "_catalog" {
		return api.Intent{
			Kind:        api.IntentSearch,
			Coord:       api.PackageCoordinate{Format: formatName},
			RemotePath:  apiRoot + "/_catalog",
			RemoteQuery: r.URL.RawQuery,
			CacheTTL:    tagTTL,
			ContentType: mediaTypeJSON,
		}, nil
	}

	ref, err := parseRef(rest)
	if err != nil {
		return api.Intent{}, err
	}
	remotePath := apiRoot + "/" + rest

	switch ref.kind {
	case refTags:
		// What tags a repository has is a question, not a document: a
		// hosting feed answers it from what it holds, a proxy asks its
		// upstream (api.Searcher, api.IntentSearch).
		return api.Intent{
			Kind:        api.IntentSearch,
			Coord:       api.PackageCoordinate{Format: formatName, Name: ref.repo},
			RemotePath:  remotePath,
			RemoteQuery: r.URL.RawQuery,
			CacheTTL:    tagTTL,
			ContentType: mediaTypeJSON,
		}, nil

	case refManifest:
		if digest, isDigest := parseDigest(ref.reference); isDigest {
			// Addressed by content: immutable, and verified against the
			// digest that is right there in the URL (invariant 5).
			return api.Intent{
				Kind:       api.IntentArtifact,
				Coord:      api.PackageCoordinate{Format: formatName, Name: ref.repo, Version: ref.reference},
				RemotePath: remotePath,
				Checksum:   digest,
				Accept:     manifestAccept,
			}, nil
		}
		// A tag is a pointer that moves, so it is mutable metadata: cached
		// with a TTL, served stale when the upstream is down (invariant 6),
		// and — where the feed hosts it — read from the coordinate the
		// publisher last pointed it at.
		return api.Intent{
			Kind:       api.IntentMetadata,
			Coord:      api.PackageCoordinate{Format: formatName, Name: ref.repo, Version: ref.reference},
			RemotePath: remotePath,
			CacheTTL:   tagTTL,
			Accept:     manifestAccept,
		}, nil

	case refBlob:
		digest, isDigest := parseDigest(ref.reference)
		if !isDigest {
			return api.Intent{}, api.NotFoundf("blob reference %q is not a digest", ref.reference)
		}
		return api.Intent{
			Kind:        api.IntentArtifact,
			Coord:       api.PackageCoordinate{Format: formatName, Name: ref.repo},
			RemotePath:  remotePath,
			Checksum:    digest,
			ContentType: mediaTypeOctetStream,
		}, nil

	default:
		return api.Intent{}, api.NotFoundf("unsupported registry path: %q", p)
	}
}

// RewriteMetadata implements api.FormatModule.
//
// It rewrites nothing, and that is load-bearing rather than lazy: a
// manifest's identity IS the sha256 of these exact bytes, so re-encoding it
// — even to identical JSON with different spacing — would change the digest
// every client checks it against and break the image. Nothing inside points
// at a host either; layers are named by digest, not by URL.
//
// What it does do is refuse a body that is not a JSON document, so an
// upstream error page cannot be cached as if it were an image.
func (Module) RewriteMetadata(_ api.Feed, upstreamBody []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(upstreamBody))
	if !strings.HasPrefix(trimmed, "{") {
		return nil, fmt.Errorf("upstream answered with something that is not a registry document: %w",
			api.ErrBadRequest)
	}
	return upstreamBody, nil
}

// ---------------------------------------------------------------------------
// The path grammar

type refKind int

const (
	refNone refKind = iota
	refManifest
	refBlob
	refUpload
	refTags
)

// parsedRef is a request path taken apart: which repository, which kind of
// object, and which one.
type parsedRef struct {
	repo      string
	kind      refKind
	reference string
}

// Separators that end a repository name. A repository name may itself
// contain a segment called "blobs" or "manifests", so the LAST occurrence is
// the real separator.
const (
	sepManifests = "/manifests/"
	sepBlobs     = "/blobs/"
	sepUploads   = "/blobs/uploads/"
	sepTagsList  = "/tags/list"
)

// parseRef splits "<name>/<kind>/<reference>" the way the protocol means it.
func parseRef(rest string) (parsedRef, error) {
	if strings.HasSuffix(rest, sepTagsList) {
		repo := strings.TrimSuffix(rest, sepTagsList)
		if err := validRepo(repo); err != nil {
			return parsedRef{}, err
		}
		return parsedRef{repo: repo, kind: refTags}, nil
	}
	// Uploads first: "<name>/blobs/uploads/<id>" also contains "/blobs/",
	// and reading it as a blob would take "uploads/<id>" for a digest.
	if i := strings.LastIndex(rest, sepUploads); i >= 0 {
		repo := rest[:i]
		if err := validRepo(repo); err != nil {
			return parsedRef{}, err
		}
		return parsedRef{repo: repo, kind: refUpload, reference: rest[i+len(sepUploads):]}, nil
	}
	if i := strings.LastIndex(rest, sepManifests); i >= 0 {
		repo, reference := rest[:i], rest[i+len(sepManifests):]
		if err := validRepo(repo); err != nil {
			return parsedRef{}, err
		}
		if reference == "" || strings.Contains(reference, "/") {
			return parsedRef{}, api.NotFoundf("not a manifest reference: %q", reference)
		}
		return parsedRef{repo: repo, kind: refManifest, reference: reference}, nil
	}
	if i := strings.LastIndex(rest, sepBlobs); i >= 0 {
		repo, reference := rest[:i], rest[i+len(sepBlobs):]
		if err := validRepo(repo); err != nil {
			return parsedRef{}, err
		}
		if reference == "" || strings.Contains(reference, "/") {
			return parsedRef{}, api.NotFoundf("not a blob reference: %q", reference)
		}
		return parsedRef{repo: repo, kind: refBlob, reference: reference}, nil
	}
	return parsedRef{}, api.NotFoundf("unsupported registry path: %q", rest)
}

// validRepo checks a repository name against the grammar the spec defines.
// It is also what keeps a crafted name from becoming a storage key that
// escapes the feed.
func validRepo(name string) error {
	if name == "" || len(name) > 255 {
		return api.NotFoundf("repository name %q is not usable", name)
	}
	for _, segment := range strings.Split(name, "/") {
		if !validRepoSegment(segment) {
			return fmt.Errorf("repository name %q is not lowercase alphanumeric with separators: %w",
				name, api.ErrBadRequest)
		}
	}
	return nil
}

// validRepoSegment implements [a-z0-9]+(([._]|__|-+)[a-z0-9]+)*.
func validRepoSegment(s string) bool {
	if s == "" {
		return false
	}
	previousSeparator := true // a segment may not start with a separator
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			previousSeparator = false
		case c == '.' || c == '_' || c == '-':
			if previousSeparator && c != '-' && c != '_' {
				return false
			}
			previousSeparator = true
		default:
			return false
		}
	}
	// A segment may not end with a separator either.
	return !previousSeparator
}

// parseDigest reads "<algo>:<hex>" and reports whether it is one.
//
// Only sha256 and sha512 are accepted, which is what the spec's registered
// algorithms are; anything else would be stored under a digest this registry
// cannot verify.
func parseDigest(reference string) (api.Checksum, bool) {
	algo, hexDigest, ok := strings.Cut(reference, ":")
	if !ok {
		return api.Checksum{}, false
	}
	want := map[string]int{"sha256": 64, "sha512": 128}[algo]
	if want == 0 || len(hexDigest) != want {
		return api.Checksum{}, false
	}
	for i := 0; i < len(hexDigest); i++ {
		c := hexDigest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return api.Checksum{}, false
		}
	}
	return api.Checksum{Algo: algo, Hex: hexDigest}, true
}

// validTag implements [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}.
func validTag(tag string) bool {
	if tag == "" || len(tag) > 128 {
		return false
	}
	first := tag[0]
	alphanumeric := first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' ||
		first >= '0' && first <= '9'
	if !alphanumeric && first != '_' {
		return false
	}
	for i := 1; i < len(tag); i++ {
		c := tag[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func isManifestPath(remotePath string) bool { return strings.Contains(remotePath, sepManifests) }

func isBlobPath(remotePath string) bool {
	return strings.Contains(remotePath, sepBlobs) && !strings.Contains(remotePath, sepUploads)
}

// manifestPath is where a manifest lives inside a feed.
func manifestPath(repo, reference string) string {
	return apiRoot + "/" + repo + sepManifests + reference
}

// blobPath is where a blob lives inside a feed.
func blobPath(repo, digest string) string {
	return apiRoot + "/" + repo + sepBlobs + digest
}

// feedURL is the path a client reaches this feed's API at, which is what a
// Location header has to name.
func feedURL(feed api.Feed) string {
	return "/" + apiRoot + "/" + formatName + "/" + feed.Name
}
