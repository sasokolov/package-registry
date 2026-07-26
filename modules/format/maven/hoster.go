package maven

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // protocol checksum, not security
	"crypto/sha1" //nolint:gosec // protocol checksum, not security
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sasokolov/package-registry/core/api"
)

// maxUploadSize bounds a single PUT body.
const maxUploadSize = 2 << 30 // 2 GiB

// defaultSnapshotRetention is how many timestamped builds of one SNAPSHOT
// coordinate stay in the index.
const defaultSnapshotRetention = 5

// HandlePublish implements api.Hoster: `mvn deploy` PUTs the artifact, its
// .pom and their .sha1/.md5 sidecars, then a maven-metadata.xml.
//
//   - artifacts and poms are staged as content-addressed blobs and committed
//     through CoreServices.Publish (immutability lives there);
//   - sidecar checksums are verified against the stored digests and then
//     discarded: they are regenerated from the manifest on read;
//   - maven-metadata.xml uploads are accepted and ignored, because the feed
//     index is derived data rebuilt by Reindex (invariant 15).
func (Module) HandlePublish(ctx context.Context, feed api.Feed, r *http.Request, deps api.CoreServices) error {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		return fmt.Errorf("empty upload path: %w", api.ErrBadRequest)
	}

	intent, err := Module{}.Parse(r)
	if err != nil {
		return err
	}
	if strings.HasSuffix(intent.RemotePath, "maven-metadata.xml") {
		// Drain and ignore: Reindex owns this document.
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxUploadSize))
		return nil
	}

	if intent.WantChecksum != "" {
		return verifyUploadedChecksum(ctx, feed, intent, r, deps)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxUploadSize))
	if err != nil {
		return fmt.Errorf("read upload: %w", err)
	}
	digests := digestsOf(body)
	sha256hex := digests["sha256"]

	if err := deps.Blobs().Put(ctx, "blobs/sha256/"+sha256hex, bytes.NewReader(body),
		api.PutOpts{SHA256: sha256hex, Size: int64(len(body))}); err != nil {
		return fmt.Errorf("stage blob: %w", err)
	}

	meta := map[string]string{api.MetaEcosystem: "Maven"}
	if strings.HasSuffix(intent.RemotePath, ".pom") {
		if pomMeta, err := parsePOM(body); err == nil {
			for k, v := range pomMeta {
				meta[k] = v
			}
		}
	}

	_, err = deps.Publish(ctx, api.PublishRequest{
		Feed:      feed,
		Coord:     intent.Coord,
		Path:      intent.RemotePath,
		SHA256:    sha256hex,
		Size:      int64(len(body)),
		Checksums: digests,
		Metadata:  meta,
	})
	return err
}

// verifyUploadedChecksum checks a client-uploaded .sha1/.md5 sidecar against
// the digest the registry computed for the artifact. Mismatch means the
// upload was corrupted in flight.
func verifyUploadedChecksum(ctx context.Context, feed api.Feed, intent api.Intent, r *http.Request, deps api.CoreServices) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		return fmt.Errorf("read checksum upload: %w", err)
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return fmt.Errorf("empty checksum upload: %w", api.ErrBadRequest)
	}
	claimed := strings.ToLower(fields[0])

	manifests, err := deps.Manifests(ctx, feed, intent.RemotePath)
	if err != nil {
		return err
	}
	for _, m := range manifests {
		if m.Path != intent.RemotePath {
			continue
		}
		stored := m.Checksums[intent.WantChecksum]
		if stored == "" && intent.WantChecksum == "sha256" {
			stored = m.SHA256
		}
		if stored != "" && !strings.EqualFold(stored, claimed) {
			return fmt.Errorf("uploaded %s %s does not match stored %s: %w",
				intent.WantChecksum, claimed, stored, api.ErrChecksumMismatch)
		}
		return nil
	}
	// Checksum uploaded before its artifact: nothing to verify against.
	return nil
}

func digestsOf(body []byte) map[string]string {
	hashes := map[string]hash.Hash{
		"sha1":   sha1.New(), //nolint:gosec // protocol checksum
		"md5":    md5.New(),  //nolint:gosec // protocol checksum
		"sha256": sha256.New(),
		"sha512": sha512.New(),
	}
	out := make(map[string]string, len(hashes))
	for algo, h := range hashes {
		_, _ = h.Write(body)
		out[algo] = hex.EncodeToString(h.Sum(nil))
	}
	return out
}

// ---------------------------------------------------------------------------
// Reindex

// mavenMetadata is the generated maven-metadata.xml.
type mavenMetadata struct {
	XMLName    xml.Name   `xml:"metadata"`
	GroupID    string     `xml:"groupId"`
	ArtifactID string     `xml:"artifactId"`
	Versioning versioning `xml:"versioning"`
}

type versioning struct {
	Latest      string   `xml:"latest"`
	Release     string   `xml:"release"`
	Versions    versions `xml:"versions"`
	LastUpdated string   `xml:"lastUpdated"`
}

type versions struct {
	Version []string `xml:"version"`
}

// Reindex implements api.Hoster: rebuild every artifact's
// maven-metadata.xml from the hosted manifest set.
//
// It is a deterministic pure function of that set — the same manifests
// always produce byte-identical output — which is what lets geo replication
// replicate manifests only and rebuild indexes locally (invariant 15).
func (Module) Reindex(ctx context.Context, feed api.Feed, deps api.CoreServices) error {
	manifests, err := deps.Manifests(ctx, feed, "")
	if err != nil {
		return err
	}
	if err := reindexSnapshots(ctx, feed, deps, manifests, defaultSnapshotRetention); err != nil {
		return err
	}

	type artifactKey struct{ group, artifact string }
	byArtifact := make(map[artifactKey]map[string]bool)
	for _, m := range manifests {
		group, artifact, version, ok := splitCoordinate(m.Coord)
		if !ok {
			continue
		}
		if strings.Contains(m.Path, "/") && timestampedRE.MatchString(m.Path) {
			// Timestamped SNAPSHOT builds are listed by the version-level
			// index, not by the artifact-level one.
			version = snapshotVersionOf(version)
		}
		key := artifactKey{group, artifact}
		if byArtifact[key] == nil {
			byArtifact[key] = make(map[string]bool)
		}
		byArtifact[key][version] = true
	}

	keys := make([]artifactKey, 0, len(byArtifact))
	for k := range byArtifact {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].group != keys[j].group {
			return keys[i].group < keys[j].group
		}
		return keys[i].artifact < keys[j].artifact
	})

	for _, k := range keys {
		versionSet := byArtifact[k]
		list := make([]string, 0, len(versionSet))
		for v := range versionSet {
			list = append(list, v)
		}
		sort.Sort(byVersion(list))

		doc := mavenMetadata{
			GroupID:    k.group,
			ArtifactID: k.artifact,
			Versioning: versioning{
				Latest:   list[len(list)-1],
				Release:  latestRelease(list),
				Versions: versions{Version: list},
				// Deterministic by construction: no wall clock, so
				// reindexing twice yields byte-identical output.
				LastUpdated: lastUpdatedOf(manifests, k.group, k.artifact),
			},
		}
		body, err := xml.MarshalIndent(doc, "", "  ")
		if err != nil {
			return fmt.Errorf("encode maven-metadata.xml: %w", err)
		}
		body = append([]byte(xml.Header), body...)
		body = append(body, '\n')

		path := strings.ReplaceAll(k.group, ".", "/") + "/" + k.artifact + "/maven-metadata.xml"
		if err := deps.PutIndex(ctx, feed, path, body); err != nil {
			return err
		}
	}
	return nil
}

// lastUpdatedOf derives the index timestamp from the newest publication in
// the set (Maven's yyyyMMddHHmmss form) — data, not wall clock.
func lastUpdatedOf(manifests []api.HostedManifest, group, artifact string) string {
	var newest time.Time
	for _, m := range manifests {
		g, a, _, ok := splitCoordinate(m.Coord)
		if !ok || g != group || a != artifact {
			continue
		}
		if m.PublishedAt.After(newest) {
			newest = m.PublishedAt
		}
	}
	if newest.IsZero() {
		return "00000000000000"
	}
	return newest.UTC().Format("20060102150405")
}

func splitCoordinate(c api.PackageCoordinate) (group, artifact, version string, ok bool) {
	if c.Version == "" {
		return "", "", "", false
	}
	group, artifact, found := strings.Cut(c.Name, ":")
	if !found || group == "" || artifact == "" {
		return "", "", "", false
	}
	return group, artifact, c.Version, true
}

func latestRelease(sorted []string) string {
	for i := len(sorted) - 1; i >= 0; i-- {
		if !strings.HasSuffix(sorted[i], "-SNAPSHOT") {
			return sorted[i]
		}
	}
	return ""
}

// byVersion orders Maven versions numerically segment by segment, falling
// back to lexicographic comparison for non-numeric segments.
type byVersion []string

func (v byVersion) Len() int      { return len(v) }
func (v byVersion) Swap(i, j int) { v[i], v[j] = v[j], v[i] }
func (v byVersion) Less(i, j int) bool {
	return compareVersions(v[i], v[j]) < 0
}

func compareVersions(a, b string) int {
	as := splitVersion(a)
	bs := splitVersion(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xn, xIsNum := atoi(x)
		yn, yIsNum := atoi(y)
		switch {
		case xIsNum && yIsNum:
			if xn != yn {
				if xn < yn {
					return -1
				}
				return 1
			}
		case x != y:
			// Maven ordering: an extra numeric segment sorts after
			// ("1.0" < "1.0.1"), while a qualifier sorts before the plain
			// release ("1.0-rc1" < "1.0").
			if x == "" {
				if yIsNum {
					return -1
				}
				return 1
			}
			if y == "" {
				if xIsNum {
					return 1
				}
				return -1
			}
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func splitVersion(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || r == '+'
	})
}

func atoi(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// ---------------------------------------------------------------------------
// Metadata extraction (api.MetadataSource)

// MetadataIntent points at the coordinate's .pom, which carries the license
// and other declared metadata.
func (Module) MetadataIntent(_ api.Feed, coord api.PackageCoordinate) (api.Intent, bool) {
	group, artifact, found := strings.Cut(coord.Name, ":")
	if !found || coord.Version == "" {
		return api.Intent{}, false
	}
	path := strings.ReplaceAll(group, ".", "/") + "/" + artifact + "/" + coord.Version +
		"/" + artifact + "-" + coord.Version + ".pom"
	return api.Intent{
		Kind:           api.IntentArtifact,
		Coord:          coord,
		RemotePath:     path,
		RemoteChecksum: api.ChecksumSource{Algo: "sha1", Path: path + ".sha1"},
		ContentType:    "application/xml",
	}, true
}

// ExtractMetadata parses a .pom into canonical keys. A pom describes one
// coordinate, so the coordinate argument is not needed here.
func (Module) ExtractMetadata(_ api.PackageCoordinate, body []byte) (map[string]string, error) {
	return parsePOM(body)
}

type pomDoc struct {
	Licenses []struct {
		Name string `xml:"name"`
	} `xml:"licenses>license"`
}

func parsePOM(body []byte) (map[string]string, error) {
	var doc pomDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse pom: %w", err)
	}
	meta := map[string]string{api.MetaEcosystem: "Maven"}
	names := make([]string, 0, len(doc.Licenses))
	for _, l := range doc.Licenses {
		if name := strings.TrimSpace(l.Name); name != "" {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		meta[api.MetaLicense] = strings.Join(names, " OR ")
	}
	return meta, nil
}

// snapshotVersionOf normalises a timestamped build back to its SNAPSHOT
// version for the artifact-level index.
func snapshotVersionOf(version string) string { return version }
