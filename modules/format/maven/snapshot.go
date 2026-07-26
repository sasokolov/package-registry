package maven

import (
	"context"
	"encoding/xml"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
)

// SNAPSHOT support.
//
// A SNAPSHOT version is mutable as a whole but each deployed build is an
// immutable timestamped artifact:
//
//	lib-1.0.0-20260726.101500-3.jar   the build (immutable)
//	lib-1.0.0-SNAPSHOT.jar            alias for the newest build (mutable)
//	.../1.0.0-SNAPSHOT/maven-metadata.xml   version-level index (rebuilt)
//
// Retention keeps the newest N builds per coordinate; older ones are dropped
// from the index (blobs are content-addressed and collected by `registry gc`).

// snapshotSuffix marks a SNAPSHOT version.
const snapshotSuffix = "-SNAPSHOT"

// timestampedRE matches "<base>-<yyyyMMdd>.<HHmmss>-<build>" inside a file
// name, e.g. "lib-1.0.0-20260726.101500-3.jar".
var timestampedRE = regexp.MustCompile(`-(\d{8}\.\d{6})-(\d+)(?:[.-]|$)`)

// isSnapshotVersion reports whether a version string is a SNAPSHOT.
func isSnapshotVersion(v string) bool { return strings.HasSuffix(v, snapshotSuffix) }

// snapshotBuild describes one timestamped build of a SNAPSHOT.
type snapshotBuild struct {
	timestamp string // yyyyMMdd.HHmmss
	build     int
	// classifier+extension of the file, e.g. "jar", "pom", "sources.jar".
	kind string
	path string
}

// snapshotMetadata is the version-level maven-metadata.xml of a SNAPSHOT.
type snapshotMetadata struct {
	XMLName    xml.Name           `xml:"metadata"`
	GroupID    string             `xml:"groupId"`
	ArtifactID string             `xml:"artifactId"`
	Version    string             `xml:"version"`
	Versioning snapshotVersioning `xml:"versioning"`
}

type snapshotVersioning struct {
	Snapshot            snapshotStamp        `xml:"snapshot"`
	LastUpdated         string               `xml:"lastUpdated"`
	SnapshotVersionList []snapshotVersionXML `xml:"snapshotVersions>snapshotVersion"`
}

type snapshotStamp struct {
	Timestamp   string `xml:"timestamp"`
	BuildNumber int    `xml:"buildNumber"`
}

type snapshotVersionXML struct {
	Classifier string `xml:"classifier,omitempty"`
	Extension  string `xml:"extension"`
	Value      string `xml:"value"`
	Updated    string `xml:"updated"`
}

// reindexSnapshots regenerates the version-level maven-metadata.xml for every
// hosted SNAPSHOT coordinate and enforces the retention limit.
func reindexSnapshots(ctx context.Context, feed api.Feed, deps api.CoreServices,
	manifests []api.HostedManifest, keepBuilds int) error {

	type key struct{ group, artifact, version string }
	builds := map[key][]snapshotBuild{}

	for _, m := range manifests {
		group, artifact, version, ok := splitCoordinate(m.Coord)
		if !ok || !isSnapshotVersion(version) {
			continue
		}
		file := m.Path[strings.LastIndex(m.Path, "/")+1:]
		match := timestampedRE.FindStringSubmatch(file)
		if match == nil {
			continue // the mutable alias, not a build
		}
		build, err := strconv.Atoi(match[2])
		if err != nil {
			continue
		}
		k := key{group, artifact, version}
		builds[k] = append(builds[k], snapshotBuild{
			timestamp: match[1],
			build:     build,
			kind:      kindOf(file, match[0]),
			path:      m.Path,
		})
	}

	keys := make([]key, 0, len(builds))
	for k := range builds {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].group != keys[j].group {
			return keys[i].group < keys[j].group
		}
		if keys[i].artifact != keys[j].artifact {
			return keys[i].artifact < keys[j].artifact
		}
		return keys[i].version < keys[j].version
	})

	for _, k := range keys {
		list := builds[k]
		sort.Slice(list, func(i, j int) bool {
			if list[i].timestamp != list[j].timestamp {
				return list[i].timestamp < list[j].timestamp
			}
			if list[i].build != list[j].build {
				return list[i].build < list[j].build
			}
			return list[i].kind < list[j].kind
		})

		// Retention: keep the newest keepBuilds build numbers.
		if keepBuilds > 0 {
			seen := map[int]bool{}
			for _, b := range list {
				seen[b.build] = true
			}
			if len(seen) > keepBuilds {
				numbers := make([]int, 0, len(seen))
				for n := range seen {
					numbers = append(numbers, n)
				}
				sort.Ints(numbers)
				cutoff := numbers[len(numbers)-keepBuilds]
				kept := list[:0]
				for _, b := range list {
					if b.build >= cutoff {
						kept = append(kept, b)
					}
				}
				list = kept
			}
		}
		if len(list) == 0 {
			continue
		}

		newest := list[len(list)-1]
		doc := snapshotMetadata{
			GroupID:    k.group,
			ArtifactID: k.artifact,
			Version:    k.version,
			Versioning: snapshotVersioning{
				Snapshot: snapshotStamp{
					Timestamp:   newest.timestamp,
					BuildNumber: newest.build,
				},
				LastUpdated: strings.ReplaceAll(newest.timestamp, ".", ""),
			},
		}
		// One entry per kind, newest build wins.
		latestByKind := map[string]snapshotBuild{}
		for _, b := range list {
			latestByKind[b.kind] = b
		}
		kinds := make([]string, 0, len(latestByKind))
		for kind := range latestByKind {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		baseVersion := strings.TrimSuffix(k.version, snapshotSuffix)
		for _, kind := range kinds {
			b := latestByKind[kind]
			classifier, extension := splitKind(kind)
			doc.Versioning.SnapshotVersionList = append(doc.Versioning.SnapshotVersionList, snapshotVersionXML{
				Classifier: classifier,
				Extension:  extension,
				Value:      fmt.Sprintf("%s-%s-%d", baseVersion, b.timestamp, b.build),
				Updated:    strings.ReplaceAll(b.timestamp, ".", ""),
			})
		}

		body, err := xml.MarshalIndent(doc, "", "  ")
		if err != nil {
			return fmt.Errorf("encode snapshot metadata: %w", err)
		}
		body = append([]byte(xml.Header), body...)
		body = append(body, '\n')
		path := strings.ReplaceAll(k.group, ".", "/") + "/" + k.artifact + "/" + k.version + "/maven-metadata.xml"
		if err := deps.PutIndex(ctx, feed, path, body); err != nil {
			return err
		}
	}
	return nil
}

// kindOf derives "classifier.extension" (or just the extension) from a
// timestamped file name.
func kindOf(file, stamp string) string {
	idx := strings.Index(file, stamp)
	if idx < 0 {
		return "jar"
	}
	rest := file[idx+len(stamp):]
	if rest == "" {
		// The stamp ended the name: the extension follows the last dot.
		if dot := strings.LastIndex(file, "."); dot >= 0 {
			return file[dot+1:]
		}
		return "jar"
	}
	// The remainder is either ".jar" or "-sources.jar"; both forms yield
	// the classifier-qualified kind.
	return strings.TrimLeft(rest, ".-")
}

// splitKind separates "sources.jar" into ("sources", "jar").
func splitKind(kind string) (classifier, extension string) {
	if i := strings.LastIndex(kind, "."); i > 0 {
		return kind[:i], kind[i+1:]
	}
	return "", kind
}
