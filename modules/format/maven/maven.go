// Package maven implements the FormatModule for the Maven 2 repository
// layout: /{group...}/{artifact}/{version}/{file} plus maven-metadata.xml.
//
// Release artifacts are immutable; maven-metadata.xml is mutable (SWR).
// Sidecar checksum files (.sha1/.md5/.sha256/.sha512) are served from
// digests stored at ingest, never proxied as separate upstream requests;
// artifact ingest is verified against the upstream's .sha1 document.
// SNAPSHOT versions are not proxied until Phase 5 and yield a clear 404.
package maven

import (
	"net/http"
	"strings"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

// metadataTTL bounds maven-metadata.xml freshness (SWR beyond it).
const metadataTTL = 5 * time.Minute

func init() {
	api.RegisterFormat(Module{})
}

// Module implements api.FormatModule.
type Module struct{}

// Name implements api.FormatModule.
func (Module) Name() string { return "maven" }

// Routes implements api.FormatModule.
func (Module) Routes() []api.Route {
	return []api.Route{
		{Method: http.MethodGet, Pattern: "/*"},
		{Method: http.MethodHead, Pattern: "/*"},
	}
}

var checksumExts = []string{"sha512", "sha256", "sha1", "md5"}

// Parse implements api.FormatModule.
func (Module) Parse(r *http.Request) (api.Intent, error) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		return api.Intent{}, api.NotFoundf("empty path")
	}

	wantChecksum := ""
	base := p
	for _, ext := range checksumExts {
		if strings.HasSuffix(p, "."+ext) {
			wantChecksum = ext
			base = strings.TrimSuffix(p, "."+ext)
			break
		}
	}

	segs := strings.Split(base, "/")
	last := segs[len(segs)-1]
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return api.Intent{}, api.NotFoundf("invalid path %q", p)
		}
	}

	if last == "maven-metadata.xml" {
		return parseMetadata(segs, base, wantChecksum)
	}
	return parseArtifact(segs, base, wantChecksum)
}

func parseMetadata(segs []string, base, wantChecksum string) (api.Intent, error) {
	dirs := segs[:len(segs)-1]
	if len(dirs) == 0 {
		return api.Intent{}, api.NotFoundf("metadata path has no group")
	}
	if strings.HasSuffix(dirs[len(dirs)-1], "-SNAPSHOT") {
		return api.Intent{}, api.NotFoundf(
			"SNAPSHOT versions are not proxied yet (planned in Phase 5): %s", base)
	}
	// Artifact-level metadata (.../group/artifact/maven-metadata.xml) is the
	// common case; a single directory means group-level metadata.
	name := dirs[0]
	if len(dirs) >= 2 {
		name = strings.Join(dirs[:len(dirs)-1], ".") + ":" + dirs[len(dirs)-1]
	}
	return api.Intent{
		Kind:         api.IntentMetadata,
		Coord:        api.PackageCoordinate{Format: "maven", Name: name},
		CacheTTL:     metadataTTL,
		RemotePath:   base,
		WantChecksum: wantChecksum,
		ContentType:  contentTypeFor("maven-metadata.xml", wantChecksum),
	}, nil
}

func parseArtifact(segs []string, base, wantChecksum string) (api.Intent, error) {
	// group.../artifact/version/file
	if len(segs) < 4 {
		return api.Intent{}, api.NotFoundf("not a maven artifact path: %q", base)
	}
	file := segs[len(segs)-1]
	version := segs[len(segs)-2]
	artifact := segs[len(segs)-3]
	group := strings.Join(segs[:len(segs)-3], ".")

	if strings.HasSuffix(version, "-SNAPSHOT") {
		return api.Intent{}, api.NotFoundf(
			"SNAPSHOT versions are not proxied yet (planned in Phase 5): %s:%s:%s",
			group, artifact, version)
	}

	return api.Intent{
		Kind:       api.IntentArtifact,
		Coord:      api.PackageCoordinate{Format: "maven", Name: group + ":" + artifact, Version: version},
		RemotePath: base,
		// Verify the ingest against the protocol's sibling .sha1 document
		// (invariant 5); a clean upstream 404 means "not published".
		RemoteChecksum: api.ChecksumSource{Algo: "sha1", Path: base + ".sha1"},
		WantChecksum:   wantChecksum,
		ContentType:    contentTypeFor(file, wantChecksum),
	}, nil
}

func contentTypeFor(file, wantChecksum string) string {
	if wantChecksum != "" {
		return "text/plain"
	}
	switch {
	case strings.HasSuffix(file, ".jar"), strings.HasSuffix(file, ".war"), strings.HasSuffix(file, ".ear"):
		return "application/java-archive"
	case strings.HasSuffix(file, ".pom"), strings.HasSuffix(file, ".xml"):
		return "application/xml"
	default:
		return "application/octet-stream"
	}
}

// RewriteMetadata implements api.FormatModule. Maven metadata carries no
// URLs, so it is served verbatim.
func (Module) RewriteMetadata(_ api.Feed, body []byte) ([]byte, error) {
	return body, nil
}
