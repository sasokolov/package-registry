//go:build integration

package state

import (
	"fmt"
	"testing"
	"time"
)

// The counters are what "downloaded N times" comes from, so they have to
// accumulate across flushes rather than replace each other: each replica
// reports only what it served.
func TestTrafficAccumulates(t *testing.T) {
	db := openDB(t)
	ctx := t.Context()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	feed := fmt.Sprintf("traffic-%d", time.Now().UnixNano())

	for i := 0; i < 3; i++ {
		if err := db.AddTraffic(ctx, []FeedTraffic{
			{Feed: feed, Source: "cache", Requests: 2, Bytes: 20},
		}); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := db.Traffic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, row := range rows {
		if row.Feed != feed {
			continue
		}
		found = true
		if row.Requests != 6 || row.Bytes != 60 {
			t.Errorf("got %+v, want three flushes added up", row)
		}
	}
	if !found {
		t.Fatalf("no row for %s", feed)
	}
}

// A group has no inventory of its own and so no row in a scan's report. The
// tidy-up that removes feeds nobody configured any more is told the whole
// configuration, not just the scanned part — otherwise it deletes every
// group's counters on every pass, and the numbers reset hourly for no
// visible reason.
func TestForgettingKeepsFeedsThatHaveOnlyTraffic(t *testing.T) {
	db := openDB(t)
	ctx := t.Context()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UnixNano()
	member := fmt.Sprintf("member-%d", stamp)
	group := fmt.Sprintf("group-%d", stamp)
	gone := fmt.Sprintf("gone-%d", stamp)

	if err := db.SaveUsage(ctx, []FeedUsage{{Feed: member, HostedArtifacts: 1, HostedBytes: 10}}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddTraffic(ctx, []FeedTraffic{
		{Feed: member, Source: "local", Requests: 1, Bytes: 10},
		{Feed: group, Source: "local", Requests: 5, Bytes: 50},
		{Feed: gone, Source: "local", Requests: 9, Bytes: 90},
	}); err != nil {
		t.Fatal(err)
	}

	// What a scan passes: every configured feed, including the group that
	// has no inventory — and not the feed that was removed.
	if err := db.ForgetUsage(ctx, []string{member, group}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Traffic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int64{}
	for _, row := range rows {
		seen[row.Feed] += row.Requests
	}
	if seen[group] != 5 {
		t.Errorf("the group's counters were dropped: %v", seen[group])
	}
	if seen[member] != 1 {
		t.Errorf("the member's counters were dropped: %v", seen[member])
	}
	if _, still := seen[gone]; still {
		t.Errorf("a removed feed kept its counters")
	}
}

// The inventory is replaced wholesale per feed, not added to: a scan reports
// what it found, and a feed that lost half its cache to garbage collection
// must not keep the old number.
func TestSavingUsageReplacesRatherThanAccumulates(t *testing.T) {
	db := openDB(t)
	ctx := t.Context()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	feed := fmt.Sprintf("inventory-%d", time.Now().UnixNano())

	if err := db.SaveUsage(ctx, []FeedUsage{
		{Feed: feed, CachedArtifacts: 10, CachedPackages: 5, CachedBytes: 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveUsage(ctx, []FeedUsage{
		{Feed: feed, CachedArtifacts: 4, CachedPackages: 2, CachedBytes: 400},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Feed != feed {
			continue
		}
		if row.CachedArtifacts != 4 || row.CachedBytes != 400 {
			t.Fatalf("got %+v, want the newest scan's numbers", row)
		}
		return
	}
	t.Fatalf("no row for %s", feed)
}

// The site total is one row, however many replicas write it.
func TestSiteUsageIsASingleRow(t *testing.T) {
	db := openDB(t)
	ctx := t.Context()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	if err := db.SaveSiteUsage(ctx, SiteUsage{DistinctBlobs: 3, DistinctBytes: 300}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSiteUsage(ctx, SiteUsage{DistinctBlobs: 5, DistinctBytes: 500}); err != nil {
		t.Fatal(err)
	}

	got, err := db.SiteUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.DistinctBlobs != 5 || got.DistinctBytes != 500 {
		t.Fatalf("got %+v, want the second write", got)
	}
	if got.ScannedAt.IsZero() {
		t.Error("no scan timestamp")
	}
}
