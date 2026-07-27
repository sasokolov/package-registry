package access

import "testing"

func TestMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{name: "exact", pattern: "feed/releases/maven:com.example:lib", path: "feed/releases/maven:com.example:lib", want: true},
		{name: "exact does not match a different path", pattern: "feed/releases/a", path: "feed/releases/b"},
		{name: "a trailing star takes the remainder", pattern: "feed/releases/*", path: "feed/releases/maven:com.example:lib@1.0.0", want: true},
		{name: "a trailing star also matches a prefix within a segment", pattern: "feed/releases/maven:com.example:*", path: "feed/releases/maven:com.example:lib@1.0.0", want: true},
		{name: "a prefix that does not match", pattern: "feed/releases/maven:com.acme:*", path: "feed/releases/maven:com.example:lib", want: false},
		{name: "a star does not escape its feed", pattern: "feed/releases/*", path: "feed/other/thing", want: false},
		{name: "plus matches one segment", pattern: "feed/+/maven:com.example:lib", path: "feed/releases/maven:com.example:lib", want: true},
		{name: "plus does not match two segments", pattern: "feed/+", path: "feed/releases/extra", want: false},
		{name: "plus and star together", pattern: "feed/+/maven:com.example:*", path: "feed/anything/maven:com.example:lib@2.0", want: true},
		{name: "a scoped npm coordinate has its own slash", pattern: "feed/npm-hosted/*", path: "feed/npm-hosted/npm:@scope/pkg@1.0.0", want: true},
		{name: "sys paths match too", pattern: "sys/*", path: "sys/config", want: true},
		{name: "a longer path does not match an exact shorter one", pattern: "sys/config", path: "sys/config/feeds", want: false},
		{name: "the feed itself matches its own star rule", pattern: "feed/releases/*", path: "feed/releases", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, got := match(tc.pattern, tc.path)
			if got != tc.want {
				t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

// Specificity is what lets a narrow rule be a deliberate exception to a
// broad one, so the ordering has to be the one an operator would predict.
func TestSpecificityOrdering(t *testing.T) {
	const path = "feed/releases/maven:com.example:lib@1.0.0"

	tests := []struct {
		name       string
		more, less string
	}{
		{name: "exact beats a prefix", more: path, less: "feed/releases/*"},
		{name: "a longer prefix beats a shorter one", more: "feed/releases/maven:com.example:*", less: "feed/releases/*"},
		{name: "a literal segment beats a plus", more: "feed/releases/*", less: "feed/+/*"},
		{name: "fewer pluses win", more: "feed/+/maven:com.example:lib@1.0.0", less: "feed/+/+"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			moreScore, ok := match(tc.more, path)
			if !ok {
				t.Fatalf("%q does not match %q", tc.more, path)
			}
			lessScore, ok := match(tc.less, path)
			if !ok {
				t.Fatalf("%q does not match %q", tc.less, path)
			}
			if !moreScore.beats(lessScore) {
				t.Errorf("%q should be more specific than %q", tc.more, tc.less)
			}
			if lessScore.beats(moreScore) {
				t.Errorf("%q should not beat %q", tc.less, tc.more)
			}
		})
	}
}
