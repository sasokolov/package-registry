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

// The leaderboard has to add up across flushes and sort by what it claims to.
func TestTopPackagesAccumulateAndSort(t *testing.T) {
	db := openDB(t)
	ctx := t.Context()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	feed := fmt.Sprintf("top-%d", time.Now().UnixNano())

	for i := 0; i < 3; i++ {
		if err := db.AddPackageDownloads(ctx, []PackageDownload{
			{Feed: feed, Coordinate: "npm:popular@1.0.0", Downloads: 5, Bytes: 50},
			{Feed: feed, Coordinate: "npm:rare@1.0.0", Downloads: 1, Bytes: 10},
		}); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := db.TopPackages(ctx, feed, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	if rows[0].Coordinate != "npm:popular@1.0.0" {
		t.Errorf("first is %q, want the most downloaded", rows[0].Coordinate)
	}
	if rows[0].Downloads != 15 || rows[0].Bytes != 150 {
		t.Errorf("got %+v, want three flushes added up", rows[0])
	}
}

// The same coordinate in two feeds stays two rows: they are different objects
// with different access rules, and merging them would hide which feed is
// doing the work.
func TestTopPackagesKeepFeedsApart(t *testing.T) {
	db := openDB(t)
	ctx := t.Context()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UnixNano()
	one := fmt.Sprintf("one-%d", stamp)
	two := fmt.Sprintf("two-%d", stamp)

	if err := db.AddPackageDownloads(ctx, []PackageDownload{
		{Feed: one, Coordinate: "npm:shared@1.0.0", Downloads: 3},
		{Feed: two, Coordinate: "npm:shared@1.0.0", Downloads: 7},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.TopPackages(ctx, one, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Downloads != 3 {
		t.Fatalf("got %+v, want only this feed's 3", rows)
	}
}

// Pruning bounds the table on a proxy that has seen a million coordinates go
// by. It must keep the top — that is the whole list anyone reads — and it
// must not throw away something downloaded recently, or a package on its way
// up gets knocked back to zero before it arrives.
func TestPruningKeepsTheTopAndTheRecent(t *testing.T) {
	db := openDB(t)
	ctx := t.Context()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	feed := fmt.Sprintf("prune-%d", time.Now().UnixNano())

	var deltas []PackageDownload
	for i := 0; i < 10; i++ {
		deltas = append(deltas, PackageDownload{
			Feed: feed, Coordinate: fmt.Sprintf("npm:pkg-%02d@1.0.0", i), Downloads: int64(i + 1),
		})
	}
	if err := db.AddPackageDownloads(ctx, deltas); err != nil {
		t.Fatal(err)
	}

	// Everything here was written a moment ago, so an active window that
	// covers it must protect all of it however small the keep is.
	dropped, err := db.PrunePackageDownloads(ctx, 3, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Fatalf("pruning dropped %d recent rows", dropped)
	}

	// With no active window, only the top three survive.
	if _, err := db.PrunePackageDownloads(ctx, 3, 0); err != nil {
		t.Fatal(err)
	}
	rows, err := db.TopPackages(ctx, feed, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("kept %d rows, want 3: %+v", len(rows), rows)
	}
	if rows[0].Coordinate != "npm:pkg-09@1.0.0" {
		t.Errorf("kept %q at the top, want the most downloaded", rows[0].Coordinate)
	}
}
