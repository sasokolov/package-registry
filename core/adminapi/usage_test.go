package adminapi

import (
	"testing"
	"time"

	"github.com/fondaco-dev/fondaco/core/config"
	"github.com/fondaco-dev/fondaco/core/state"
	"github.com/fondaco-dev/fondaco/core/usage"
)

func report(t *testing.T, cfg *config.Config, inv []state.FeedUsage, traffic []state.FeedTraffic) map[string]FeedUsage {
	t.Helper()
	out := map[string]FeedUsage{}
	for _, u := range buildUsage(cfg, inv, traffic).feeds {
		out[u.Feed] = u
	}
	return out
}

// The two halves only mean something together: cached bytes say what a proxy
// costs, downloads say whether it earned it.
func TestAProxyFeedReportsWhatItHoldsAndWhatItSaved(t *testing.T) {
	cfg := &config.Config{Feeds: []config.FeedConfig{
		{Name: "npmjs", Format: "npm", Upstream: "https://registry.npmjs.org"},
	}}
	inv := []state.FeedUsage{{
		Feed: "npmjs", CachedArtifacts: 40, CachedPackages: 12, CachedBytes: 1000, SharedBytes: 100,
	}}
	traffic := []state.FeedTraffic{
		{Feed: "npmjs", Source: "cache", Requests: 90, Bytes: 9000},
		{Feed: "npmjs", Source: "upstream", Requests: 10, Bytes: 1000},
		{Feed: "npmjs", Source: usage.SourceIngest, Requests: 10, Bytes: 1000},
	}

	u := report(t, cfg, inv, traffic)["npmjs"]
	if u.Kind != kindProxy {
		t.Errorf("kind = %q, want proxy", u.Kind)
	}
	if u.Packages != 12 || u.Artifacts != 40 || u.Bytes != 1000 {
		t.Errorf("inventory = %+v", u)
	}
	// The ingest row is not a download: counting it would report 110.
	if u.Downloads != 100 || u.BytesServed != 10000 {
		t.Errorf("downloads = %d/%d bytes, want 100/10000", u.Downloads, u.BytesServed)
	}
	if u.UpstreamBytes != 1000 || u.BytesSaved != 9000 {
		t.Errorf("upstream = %d, saved = %d, want 1000 and 9000", u.UpstreamBytes, u.BytesSaved)
	}
	if u.HitRatio == nil || *u.HitRatio != 0.9 {
		t.Errorf("hit ratio = %v, want 0.9", u.HitRatio)
	}
}

// A hosted feed has no upstream, so a hit ratio would be a ratio of nothing.
// Reporting 100% would read as a compliment the feed did not earn.
func TestAHostedFeedHasNoHitRatio(t *testing.T) {
	cfg := &config.Config{Feeds: []config.FeedConfig{
		{Name: "releases", Format: "maven", Hosted: true},
	}}
	inv := []state.FeedUsage{{Feed: "releases", HostedArtifacts: 5, HostedPackages: 2, HostedBytes: 500}}
	traffic := []state.FeedTraffic{{Feed: "releases", Source: "local", Requests: 7, Bytes: 700}}

	u := report(t, cfg, inv, traffic)["releases"]
	if u.Kind != kindHosted {
		t.Errorf("kind = %q, want hosted", u.Kind)
	}
	if u.HitRatio != nil {
		t.Errorf("hit ratio = %v, want none", *u.HitRatio)
	}
	if u.Downloads != 7 || u.Packages != 2 {
		t.Errorf("usage = %+v", u)
	}
}

// A group stores nothing, so its numbers are its members' — shown, because
// "what can I get from this URL" is a real question, and marked, because
// adding them to a site total would count everything twice.
func TestAGroupBorrowsItsMembersNumbers(t *testing.T) {
	cfg := &config.Config{Feeds: []config.FeedConfig{
		{Name: "npm-hosted", Format: "npm", Hosted: true},
		{Name: "npmjs", Format: "npm", Upstream: "https://registry.npmjs.org"},
		{Name: "npm-public", Format: "npm", Members: []string{"npm-hosted", "npmjs"}},
	}}
	inv := []state.FeedUsage{
		{Feed: "npm-hosted", HostedArtifacts: 3, HostedPackages: 3, HostedBytes: 300},
		{Feed: "npmjs", CachedArtifacts: 7, CachedPackages: 5, CachedBytes: 700},
	}
	traffic := []state.FeedTraffic{
		{Feed: "npm-public", Source: "local", Requests: 20, Bytes: 2000},
		{Feed: "npm-hosted", Source: "local", Requests: 12, Bytes: 1200},
	}

	all := report(t, cfg, inv, traffic)
	g := all["npm-public"]
	if g.Kind != kindGroup || !g.Aggregated {
		t.Errorf("group = %+v, want an aggregated group", g)
	}
	if g.Packages != 8 || g.Artifacts != 10 || g.Bytes != 1000 {
		t.Errorf("group inventory = %+v, want its members' sum", g)
	}
	// Its own traffic is real and its own: it is what the group URL was
	// asked for, not what its members were asked for directly.
	if g.Downloads != 20 {
		t.Errorf("group downloads = %d, want 20", g.Downloads)
	}
	if all["npm-hosted"].Downloads != 12 {
		t.Errorf("member downloads = %d, want its own 12", all["npm-hosted"].Downloads)
	}
}

// A request through a group is one request. It is counted on the member that
// answered it and shown on the group as well, so the site total must add only
// one of the two — otherwise every group in the configuration inflates the
// site's traffic by the amount it is used.
func TestSiteTotalsCountAGroupRequestOnce(t *testing.T) {
	cfg := &config.Config{Feeds: []config.FeedConfig{
		{Name: "npm-hosted", Format: "npm", Hosted: true},
		{Name: "npm-public", Format: "npm", Members: []string{"npm-hosted"}},
	}}
	inv := []state.FeedUsage{{Feed: "npm-hosted", HostedArtifacts: 3, HostedPackages: 3, HostedBytes: 300}}
	traffic := []state.FeedTraffic{
		// Five requests arrived at the group URL and were answered by the
		// member, so both rows describe the same five.
		{Feed: "npm-public", Source: "local", Requests: 5, Bytes: 500},
		{Feed: "npm-hosted", Source: "local", Requests: 5, Bytes: 500},
		// And two documents the group assembled itself, which exist on no
		// member at all.
		{Feed: "npm-public", Source: usage.SourceMerged, Requests: 2, Bytes: 40},
	}

	report := buildUsage(cfg, inv, traffic)
	totals := report.totals
	if totals.Packages != 3 || totals.Artifacts != 3 {
		t.Errorf("totals = %+v, want the member's content counted once", totals)
	}
	if totals.Downloads != 7 {
		t.Errorf("downloads = %d, want 5 answered by the member plus 2 merged", totals.Downloads)
	}

	// The group still reports everything that arrived at its URL: that is
	// the question its row answers.
	for _, u := range report.feeds {
		if u.Feed == "npm-public" && u.Downloads != 7 {
			t.Errorf("group downloads = %d, want all 7 that reached the group URL", u.Downloads)
		}
	}
}

// A feed that both hosts and proxies is a real configuration, and calling it
// one or the other would hide half of what it holds.
func TestAFeedThatBothHostsAndProxiesIsReportedAsMixed(t *testing.T) {
	cfg := &config.Config{Feeds: []config.FeedConfig{
		{Name: "both", Format: "maven", Hosted: true, Upstream: "https://repo1.maven.org"},
	}}
	inv := []state.FeedUsage{{
		Feed: "both", HostedArtifacts: 2, HostedBytes: 200, CachedArtifacts: 8, CachedBytes: 800,
	}}

	u := report(t, cfg, inv, nil)["both"]
	if u.Kind != kindMixed {
		t.Errorf("kind = %q, want mixed", u.Kind)
	}
	if u.Bytes != 1000 || u.HostedBytes != 200 || u.CachedBytes != 800 {
		t.Errorf("usage = %+v", u)
	}
}

// A feed nothing has been scanned or served for reports zeroes, not absence:
// the row has to exist so the console can say "nothing here yet".
func TestAFeedWithNoDataStillAppears(t *testing.T) {
	cfg := &config.Config{Feeds: []config.FeedConfig{{Name: "fresh", Format: "npm"}}}
	all := report(t, cfg, nil, nil)
	u, ok := all["fresh"]
	if !ok {
		t.Fatal("a feed with no usage was left out of the report")
	}
	if u.Packages != 0 || u.Downloads != 0 || u.LastIngestAt != nil {
		t.Errorf("usage = %+v, want zeroes and no timestamps", u)
	}
}

// The most recent activity across sources is what "last used" means.
func TestLastDownloadIsTheMostRecentAcrossSources(t *testing.T) {
	cfg := &config.Config{Feeds: []config.FeedConfig{
		{Name: "npmjs", Format: "npm", Upstream: "https://registry.npmjs.org"},
	}}
	older := time.Now().Add(-2 * time.Hour).UTC()
	newer := time.Now().Add(-time.Minute).UTC()
	traffic := []state.FeedTraffic{
		{Feed: "npmjs", Source: "upstream", Requests: 1, Bytes: 10, LastAt: older},
		{Feed: "npmjs", Source: "cache", Requests: 1, Bytes: 10, LastAt: newer},
	}

	u := report(t, cfg, nil, traffic)["npmjs"]
	if u.LastDownloadAt == nil || !u.LastDownloadAt.Equal(newer) {
		t.Errorf("last download = %v, want %v", u.LastDownloadAt, newer)
	}
}
