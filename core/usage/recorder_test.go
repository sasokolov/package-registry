package usage

import (
	"fmt"
	"testing"

	"github.com/fondaco-dev/fondaco/core/state"
)

func totals(deltas []state.FeedTraffic) map[string]state.FeedTraffic {
	out := map[string]state.FeedTraffic{}
	for _, d := range deltas {
		key := d.Feed + "/" + d.Source
		existing := out[key]
		existing.Requests += d.Requests
		existing.Bytes += d.Bytes
		out[key] = existing
	}
	return out
}

// The counters are what "downloaded 4,120 times" comes from, so they have to
// add up across sources and survive being read.
func TestCountersAccumulatePerFeedAndSource(t *testing.T) {
	r := NewRecorder(nil, nil)
	r.Served("npmjs", "cache", "npm:pkg@1.0.0", 100)
	r.Served("npmjs", "cache", "npm:pkg@1.0.0", 50)
	r.Served("npmjs", "upstream", "npm:pkg@1.0.0", 300)
	r.Served("releases", "local", "npm:pkg@1.0.0", 10)

	got := totals(r.take())
	if c := got["npmjs/cache"]; c.Requests != 2 || c.Bytes != 150 {
		t.Errorf("npmjs/cache = %+v, want 2 requests and 150 bytes", c)
	}
	if c := got["npmjs/upstream"]; c.Requests != 1 || c.Bytes != 300 {
		t.Errorf("npmjs/upstream = %+v", c)
	}
	if c := got["releases/local"]; c.Requests != 1 || c.Bytes != 10 {
		t.Errorf("releases/local = %+v", c)
	}
	if left := r.take(); len(left) != 0 {
		t.Errorf("taking twice returned the same deltas again: %+v", left)
	}
}

// Bytes pulled from an upstream are not a download. Counting them as one
// would make a cache miss look like two downloads and ruin the hit ratio the
// number exists to support.
func TestIngestIsCountedApartFromDownloads(t *testing.T) {
	r := NewRecorder(nil, nil)
	r.Served("npmjs", "upstream", "npm:pkg@1.0.0", 300)
	r.Ingested("npmjs", 300)

	got := totals(r.take())
	if c := got["npmjs/upstream"]; c.Requests != 1 {
		t.Errorf("downloads = %+v, want one", c)
	}
	if c := got["npmjs/"+SourceIngest]; c.Bytes != 300 {
		t.Errorf("ingest = %+v, want 300 bytes", c)
	}
}

// A response whose length was not known must not invent one. Zero bytes with
// a real request is an honest gap; a guess is a number somebody will bill on.
func TestAnUnknownSizeCountsTheRequestAndNoBytes(t *testing.T) {
	r := NewRecorder(nil, nil)
	r.Served("npmjs", "cache", "npm:pkg@1.0.0", -1)

	got := totals(r.take())
	if c := got["npmjs/cache"]; c.Requests != 1 || c.Bytes != 0 {
		t.Errorf("got %+v, want one request and no bytes", c)
	}
}

// A group and the member that answered are both counted, because "nobody
// uses the group URL" and "this member holds nothing" are different problems.
func TestAGroupAndItsMemberAreCountedSeparately(t *testing.T) {
	r := NewRecorder(nil, nil)
	r.GroupServed("npm-public", "npm-hosted", "local", "npm:pkg@1.0.0", 100)
	r.Served("npm-hosted", "local", "npm:pkg@1.0.0", 100)

	got := totals(r.take())
	if c := got["npm-public/local"]; c.Requests != 1 || c.Bytes != 100 {
		t.Errorf("group = %+v", c)
	}
	if c := got["npm-hosted/local"]; c.Requests != 1 || c.Bytes != 100 {
		t.Errorf("member = %+v", c)
	}
}

// A failed flush must not throw the deltas away: an outage would then look
// like a quiet hour, which is the one thing a usage number must never do.
func TestDeltasSurviveAFailedFlush(t *testing.T) {
	r := NewRecorder(nil, nil)
	r.Served("npmjs", "cache", "npm:pkg@1.0.0", 100)

	taken := r.take()
	r.Served("npmjs", "cache", "npm:pkg@1.0.0", 25) // arrives while the write is in flight
	r.restore(taken)

	got := totals(r.take())
	if c := got["npmjs/cache"]; c.Requests != 2 || c.Bytes != 125 {
		t.Errorf("got %+v, want the restored delta folded into the new one", c)
	}
}

// The leaderboard is the point of counting coordinates at all.
func TestDownloadsAreCountedPerCoordinate(t *testing.T) {
	r := NewRecorder(nil, nil)
	r.Served("npmjs", "cache", "npm:left-pad@1.3.0", 100)
	r.Served("npmjs", "cache", "npm:left-pad@1.3.0", 100)
	r.Served("npmjs", "upstream", "npm:right-pad@2.0.0", 300)

	got := map[string]int64{}
	bytes := map[string]int64{}
	for _, p := range r.takePackages() {
		got[p.Feed+"/"+p.Coordinate] += p.Downloads
		bytes[p.Feed+"/"+p.Coordinate] += p.Bytes
	}
	if got["npmjs/npm:left-pad@1.3.0"] != 2 || bytes["npmjs/npm:left-pad@1.3.0"] != 200 {
		t.Errorf("left-pad = %d downloads, %d bytes", got["npmjs/npm:left-pad@1.3.0"],
			bytes["npmjs/npm:left-pad@1.3.0"])
	}
	if got["npmjs/npm:right-pad@2.0.0"] != 1 {
		t.Errorf("right-pad = %d", got["npmjs/npm:right-pad@2.0.0"])
	}
	if left := r.takePackages(); len(left) != 0 {
		t.Errorf("taking twice returned the deltas again: %+v", left)
	}
}

// A response with no coordinate — a metadata document, a checksum sidecar —
// is still a download of the feed, and still not a package. Listing metadata
// would produce a leaderboard of what people looked up rather than of what
// they installed.
func TestResponsesWithoutACoordinateAreNotOnTheLeaderboard(t *testing.T) {
	r := NewRecorder(nil, nil)
	r.Served("npmjs", "cache", "", 100)

	if pkgs := r.takePackages(); len(pkgs) != 0 {
		t.Errorf("got %+v, want nothing on the leaderboard", pkgs)
	}
	if c := totals(r.take())["npmjs/cache"]; c.Requests != 1 {
		t.Errorf("the feed lost the request too: %+v", c)
	}
}

// A group answers "what do people pull through this URL", so it keeps its own
// leaderboard alongside the member's.
func TestAGroupKeepsItsOwnLeaderboard(t *testing.T) {
	r := NewRecorder(nil, nil)
	r.GroupServed("npm-public", "npmjs", "cache", "npm:left-pad@1.3.0", 100)
	r.Served("npmjs", "cache", "npm:left-pad@1.3.0", 100)

	seen := map[string]int64{}
	for _, p := range r.takePackages() {
		seen[p.Feed] += p.Downloads
	}
	if seen["npm-public"] != 1 || seen["npmjs"] != 1 {
		t.Errorf("got %v, want one on each", seen)
	}
}

// An unbounded map in the request path is a way to be killed by a crawler.
// The cap keeps the top of the list — which is what anyone reads — and drops
// the newcomers, while the feed's own counters stay exact because they do not
// grow with cardinality.
func TestTheLeaderboardIsBounded(t *testing.T) {
	r := NewRecorder(nil, nil)
	for i := 0; i < maxTrackedPackages+500; i++ {
		r.Served("npmjs", "cache", fmt.Sprintf("npm:pkg-%d@1.0.0", i), 10)
	}

	packages := r.takePackages()
	if len(packages) > maxTrackedPackages {
		t.Errorf("tracked %d coordinates, want at most %d", len(packages), maxTrackedPackages)
	}
	// The feed itself counted every one of them.
	if c := totals(r.take())["npmjs/cache"]; c.Requests != int64(maxTrackedPackages+500) {
		t.Errorf("the feed counter was capped too: %+v", c)
	}
}

// The leaderboard is retried on its own after a failed flush, so a failure
// there cannot double-count the feed counters that already went in.
func TestPackageDeltasSurviveAFailedFlush(t *testing.T) {
	r := NewRecorder(nil, nil)
	r.Served("npmjs", "cache", "npm:left-pad@1.3.0", 100)

	taken := r.takePackages()
	r.Served("npmjs", "cache", "npm:left-pad@1.3.0", 25)
	r.restorePackages(taken)

	var downloads int64
	for _, p := range r.takePackages() {
		downloads += p.Downloads
	}
	if downloads != 2 {
		t.Errorf("downloads = %d, want the restored delta folded in", downloads)
	}
}
