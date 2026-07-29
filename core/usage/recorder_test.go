package usage

import (
	"testing"

	"github.com/sasokolov/package-registry/core/state"
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
	r.Served("npmjs", "cache", 100)
	r.Served("npmjs", "cache", 50)
	r.Served("npmjs", "upstream", 300)
	r.Served("releases", "local", 10)

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
	r.Served("npmjs", "upstream", 300)
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
	r.Served("npmjs", "cache", -1)

	got := totals(r.take())
	if c := got["npmjs/cache"]; c.Requests != 1 || c.Bytes != 0 {
		t.Errorf("got %+v, want one request and no bytes", c)
	}
}

// A group and the member that answered are both counted, because "nobody
// uses the group URL" and "this member holds nothing" are different problems.
func TestAGroupAndItsMemberAreCountedSeparately(t *testing.T) {
	r := NewRecorder(nil, nil)
	r.GroupServed("npm-public", "npm-hosted", "local", 100)
	r.Served("npm-hosted", "local", 100)

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
	r.Served("npmjs", "cache", 100)

	taken := r.take()
	r.Served("npmjs", "cache", 25) // arrives while the write is in flight
	r.restore(taken)

	got := totals(r.take())
	if c := got["npmjs/cache"]; c.Requests != 2 || c.Bytes != 125 {
		t.Errorf("got %+v, want the restored delta folded into the new one", c)
	}
}
