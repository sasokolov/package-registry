package config

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sasokolov/package-registry/core/api"
)

// Two formats: one that can merge the documents listing what exists, one
// that cannot. The difference is the whole rule about which formats may be
// grouped, so both have to exist to test it.

type groupableFormat struct{}

func (groupableFormat) Name() string        { return "groupable" }
func (groupableFormat) Routes() []api.Route { return nil }
func (groupableFormat) Parse(*http.Request) (api.Intent, error) {
	return api.Intent{}, nil
}
func (groupableFormat) RewriteMetadata(_ api.Feed, b []byte) ([]byte, error) { return b, nil }
func (groupableFormat) MergeableIntent(api.Intent) bool                      { return true }
func (groupableFormat) Merge(api.Feed, api.Intent, []api.GroupPart) ([]byte, error) {
	return nil, nil
}

type plainFormat struct{}

func (plainFormat) Name() string        { return "plain" }
func (plainFormat) Routes() []api.Route { return nil }
func (plainFormat) Parse(*http.Request) (api.Intent, error) {
	return api.Intent{}, nil
}
func (plainFormat) RewriteMetadata(_ api.Feed, b []byte) ([]byte, error) { return b, nil }

var registerGroupFormats sync.Once

func groupConfig(t *testing.T, feeds []FeedConfig) *Config {
	t.Helper()
	registerGroupFormats.Do(func() {
		api.RegisterFormat(groupableFormat{})
		api.RegisterFormat(plainFormat{})
	})
	return &Config{
		Site:    SiteConfig{Name: "test"},
		Server:  ServerConfig{Listen: ":8080"},
		Storage: StorageConfig{Type: StorageFS, FS: FSConfig{Path: "/tmp/x"}},
		Feeds:   feeds,
	}
}

func groupErr(t *testing.T, feeds []FeedConfig) string {
	t.Helper()
	err := groupConfig(t, feeds).Validate()
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestValidGroupIsAccepted(t *testing.T) {
	if got := groupErr(t, []FeedConfig{
		{Name: "proxy", Format: "groupable", Upstream: "https://example.com"},
		{Name: "local", Format: "groupable", Hosted: true},
		{Name: "all", Format: "groupable", Members: []string{"local", "proxy"}},
	}); got != "" {
		t.Fatalf("a valid group was refused: %s", got)
	}
}

func TestGroupsAreRefusedWhenTheyCannotBeRight(t *testing.T) {
	tests := []struct {
		name  string
		feeds []FeedConfig
		want  string
	}{
		{
			name: "a format that cannot merge would hide versions",
			feeds: []FeedConfig{
				{Name: "proxy", Format: "plain", Upstream: "https://example.com"},
				{Name: "all", Format: "plain", Members: []string{"proxy"}},
			},
			want: "does not support groups",
		},
		{
			name: "a member that does not exist",
			feeds: []FeedConfig{
				{Name: "all", Format: "groupable", Members: []string{"ghost"}},
			},
			want: `no feed named "ghost"`,
		},
		{
			name: "a member of another format",
			feeds: []FeedConfig{
				{Name: "other", Format: "plain", Upstream: "https://example.com"},
				{Name: "all", Format: "groupable", Members: []string{"other"}},
			},
			want: "can only contain its own format",
		},
		{
			name: "a group that proxies as well",
			feeds: []FeedConfig{
				{Name: "proxy", Format: "groupable", Upstream: "https://example.com"},
				{Name: "all", Format: "groupable", Upstream: "https://example.com", Members: []string{"proxy"}},
			},
			want: "a group cannot have an upstream",
		},
		{
			name: "a group that hosts as well",
			feeds: []FeedConfig{
				{Name: "proxy", Format: "groupable", Upstream: "https://example.com"},
				{Name: "all", Format: "groupable", Hosted: true, Members: []string{"proxy"}},
			},
			want: "a group cannot be hosted",
		},
		{
			name: "publishers on a group",
			feeds: []FeedConfig{
				{Name: "local", Format: "groupable", Hosted: true},
				{Name: "all", Format: "groupable", Publishers: []string{"token:ci"}, Members: []string{"local"}},
			},
			want: "a group cannot have publishers",
		},
		{
			name: "a group containing itself",
			feeds: []FeedConfig{
				{Name: "all", Format: "groupable", Members: []string{"all"}},
			},
			want: "cannot contain itself",
		},
		{
			name: "two groups pointing at each other",
			feeds: []FeedConfig{
				{Name: "a", Format: "groupable", Members: []string{"b"}},
				{Name: "b", Format: "groupable", Members: []string{"a"}},
			},
			want: "cycle",
		},
		{
			name: "the same member twice",
			feeds: []FeedConfig{
				{Name: "proxy", Format: "groupable", Upstream: "https://example.com"},
				{Name: "all", Format: "groupable", Members: []string{"proxy", "proxy"}},
			},
			want: "listed twice",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := groupErr(t, tc.feeds)
			if got == "" {
				t.Fatalf("accepted a configuration that cannot be right; wanted %q", tc.want)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("error = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// Nesting is allowed — a public group over an internal group plus a proxy is
// a real shape — but it has to terminate.
func TestNestingIsAllowedButBounded(t *testing.T) {
	feeds := []FeedConfig{
		{Name: "proxy", Format: "groupable", Upstream: "https://example.com"},
		{Name: "g0", Format: "groupable", Members: []string{"proxy"}},
	}
	for i := 1; i <= maxGroupDepth; i++ {
		feeds = append(feeds, FeedConfig{
			Name: "g" + string(rune('0'+i)), Format: "groupable",
			Members: []string{"g" + string(rune('0'+i-1))},
		})
	}
	if got := groupErr(t, feeds); got != "" {
		t.Fatalf("nesting within the limit was refused: %s", got)
	}

	feeds = append(feeds, FeedConfig{
		Name: "toodeep", Format: "groupable",
		Members: []string{"g" + string(rune('0'+maxGroupDepth))},
	})
	if got := groupErr(t, feeds); !strings.Contains(got, "nest deeper") {
		t.Fatalf("unbounded nesting was accepted: %q", got)
	}
}

// A group serves without an upstream and without hosting anything, which the
// "a feed must do something" rule has to allow.
func TestAGroupCountsAsServingSomething(t *testing.T) {
	if got := groupErr(t, []FeedConfig{
		{Name: "proxy", Format: "groupable", Upstream: "https://example.com"},
		{Name: "all", Format: "groupable", Members: []string{"proxy"}},
	}); strings.Contains(got, "needs an upstream") {
		t.Fatalf("a group was told it serves nothing: %s", got)
	}
}
