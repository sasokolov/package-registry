// Package semver orders version strings the way npm and Helm do.
//
// It lives here rather than in one of the format modules because two of them
// need exactly the same answer, and a second copy of a comparison is a second
// chance for two parts of one registry to disagree about which version is
// newest. Maven is deliberately not a user: its version scheme is its own,
// and pretending otherwise would be worse than duplication.
package semver

import "strings"

// Compare orders two semantic versions, negative when a sorts before b.
//
// It implements the part of semver that matters to a registry — numeric
// segments compared as numbers, and a pre-release losing to the release of
// the same version — without pulling in a dependency for it. Build metadata
// is not ordered by semver and is not ordered here either.
func Compare(a, b string) int {
	aCore, aPre, _ := strings.Cut(a, "-")
	bCore, bPre, _ := strings.Cut(b, "-")

	aSegs, bSegs := strings.Split(aCore, "."), strings.Split(bCore, ".")
	for i := 0; i < len(aSegs) || i < len(bSegs); i++ {
		if c := compareNumericSegment(segmentAt(aSegs, i), segmentAt(bSegs, i)); c != 0 {
			return c
		}
	}
	switch {
	case aPre == "" && bPre == "":
		return 0
	case aPre == "":
		return 1 // a release outranks a pre-release of the same version
	case bPre == "":
		return -1
	}
	return strings.Compare(aPre, bPre)
}

func segmentAt(segs []string, i int) string {
	if i < len(segs) {
		return segs[i]
	}
	return "0"
}

// compareNumericSegment compares two version segments numerically when both
// are numbers, and lexically otherwise.
func compareNumericSegment(a, b string) int {
	an, aOK := atoi(a)
	bn, bOK := atoi(b)
	if aOK && bOK {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a, b)
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
