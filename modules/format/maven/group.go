package maven

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strings"

	"github.com/sasokolov/package-registry/core/api"
)

// Merging for groups. maven-metadata.xml is the document that answers "what
// versions exist"; if a group answered it from the first member that had it,
// a hosted member with 1.0.0 would hide every release the proxied member
// offers, and the client would be told, with a straight face, that they do
// not exist.
//
// Everything else Maven asks for is an exact path — a .jar, a .pom, a
// sidecar checksum — and for those the first member that has it is the right
// answer, because a coordinate resolves to one artifact.

// MergeableIntent implements api.GroupMerger.
func (Module) MergeableIntent(intent api.Intent) bool {
	if intent.Kind != api.IntentMetadata {
		return false
	}
	// A checksum sidecar of the index describes one member's copy and
	// cannot describe the merged document, so it is not merged — and it is
	// not served either: a merged index has no stored digest to hand out.
	if intent.WantChecksum != "" {
		return false
	}
	return strings.HasSuffix(intent.RemotePath, "maven-metadata.xml")
}

// Merge implements api.GroupMerger: the union of the members' versions.
//
// Member order decides the identity fields (groupId, artifactId) and breaks
// ties; the version list itself is a set, sorted the way Maven sorts, so the
// result is a deterministic function of its inputs and two replicas answer
// identically.
func (Module) Merge(_ api.Feed, _ api.Intent, parts []api.GroupPart) ([]byte, error) {
	merged := mavenMetadata{}
	seen := map[string]bool{}
	var all []string
	var lastUpdated string
	var release string

	for _, part := range parts {
		var doc mavenMetadata
		if err := xml.Unmarshal(part.Body, &doc); err != nil {
			return nil, fmt.Errorf("parse maven-metadata.xml from %s: %w", part.Feed, err)
		}
		if merged.GroupID == "" {
			merged.GroupID = doc.GroupID
			merged.ArtifactID = doc.ArtifactID
		}
		for _, v := range doc.Versioning.Versions.Version {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			all = append(all, v)
		}
		// The newest lastUpdated across members is the truthful one: the
		// document as a whole was last changed when any member's was.
		if doc.Versioning.LastUpdated > lastUpdated {
			lastUpdated = doc.Versioning.LastUpdated
		}
		if doc.Versioning.Release != "" && isNewerVersion(doc.Versioning.Release, release) {
			release = doc.Versioning.Release
		}
	}

	if len(all) == 0 {
		return nil, api.NotFoundf("no member of the group lists any version")
	}
	sort.Sort(byVersion(all))

	// latest and release are recomputed rather than copied: a member's own
	// idea of "latest" is only latest within that member.
	merged.Versioning = versioning{
		Latest:      all[len(all)-1],
		Release:     latestRelease(all),
		Versions:    versions{Version: all},
		LastUpdated: lastUpdated,
	}
	if merged.Versioning.Release == "" {
		merged.Versioning.Release = release
	}

	body, err := xml.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode merged maven-metadata.xml: %w", err)
	}
	out := append([]byte(xml.Header), body...)
	return append(out, '\n'), nil
}

// isNewerVersion reports whether a sorts after b in Maven's ordering.
func isNewerVersion(a, b string) bool {
	if b == "" {
		return true
	}
	list := []string{a, b}
	sort.Sort(byVersion(list))
	return list[1] == a
}
