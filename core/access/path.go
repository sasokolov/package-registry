package access

import "strings"

// Path matching. Two wildcards, both Vault's:
//
//	*   at the end only, matches the whole remainder
//	+   one segment, between slashes
//
// That is the entire language. It is small enough that an operator can hold
// the matching rule in their head, which matters more here than expressive
// power: a rule nobody can predict the effect of is a rule nobody dares
// change.

// specificity orders competing patterns. The most specific pattern decides,
// so a narrow rule can be a deliberate exception to a broad one.
type specificity struct {
	// exact is true for a pattern with no wildcards at all. Nothing beats
	// naming the thing you mean.
	exact bool
	// pluses counts "+" segments; fewer means more specific.
	pluses int
	// literal is how many characters of the pattern are literal prefix.
	// Between two prefix patterns, the longer one is the more specific.
	literal int
	// segments is how many segments the pattern constrains; a pattern that
	// says more about the shape of the path is more specific than one that
	// stops earlier.
	segments int
}

// beats reports whether s is more specific than other.
func (s specificity) beats(other specificity) bool {
	switch {
	case s.exact != other.exact:
		return s.exact
	case s.pluses != other.pluses:
		return s.pluses < other.pluses
	case s.literal != other.literal:
		return s.literal > other.literal
	default:
		return s.segments > other.segments
	}
}

// match reports whether pattern matches path, and how specifically.
func match(pattern, path string) (specificity, bool) {
	prefix, trailing := strings.CutSuffix(pattern, "*")

	score := specificity{
		exact:    !trailing && !strings.Contains(pattern, "+"),
		segments: strings.Count(prefix, "/"),
	}

	patternSegs := strings.Split(prefix, "/")
	pathSegs := strings.Split(path, "/")

	// With a trailing "*", the pattern's last segment is a prefix of the
	// path's corresponding segment and everything after it is free.
	limit := len(patternSegs)
	if !trailing && len(patternSegs) != len(pathSegs) {
		return specificity{}, false
	}
	if trailing && len(pathSegs) < limit {
		// "feed/x/*" must still match "feed/x/" style paths only when the
		// path reaches that depth; a shorter path does not.
		if len(pathSegs) < limit-1 {
			return specificity{}, false
		}
	}

	for i := 0; i < limit; i++ {
		segment := patternSegs[i]
		last := i == limit-1

		if segment == "+" {
			if i >= len(pathSegs) {
				return specificity{}, false
			}
			score.pluses++
			continue
		}
		if i >= len(pathSegs) {
			// The pattern wants a segment the path does not have. Only a
			// trailing "*" on an empty last segment tolerates that, which
			// is the "feed/x/*" against "feed/x" case.
			if trailing && last && segment == "" {
				continue
			}
			return specificity{}, false
		}
		if last && trailing {
			if !strings.HasPrefix(pathSegs[i], segment) {
				return specificity{}, false
			}
			score.literal += len(segment)
			continue
		}
		if pathSegs[i] != segment {
			return specificity{}, false
		}
		score.literal += len(segment)
	}
	return score, true
}
