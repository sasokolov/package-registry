//go:build integration

package state

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ListHosted with an empty feed must return every feed: snapshot bootstrap,
// projection repair and backfill all depend on it, and a filter that
// matches nothing turns each of them into a silent no-op.
func TestListHostedAcrossAllFeeds(t *testing.T) {
	db := openDB(t)
	ctx := t.Context()
	// openDB does not migrate; this test needs the current schema.
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UnixNano()
	feedA := fmt.Sprintf("feed-a-%d", stamp)
	feedB := fmt.Sprintf("feed-b-%d", stamp)

	for _, row := range []HostedRow{
		{Feed: feedA, Path: "a/1.0.0/a.jar", Coordinate: "maven:a@1.0.0", SHA256: strings.Repeat("a", 64), Size: 1},
		{Feed: feedB, Path: "b/1.0.0/b.jar", Coordinate: "maven:b@1.0.0", SHA256: strings.Repeat("b", 64), Size: 2},
	} {
		if _, err := db.InsertHosted(ctx, row); err != nil {
			t.Fatal(err)
		}
	}

	all, err := db.ListHosted(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var sawA, sawB bool
	for _, r := range all {
		sawA = sawA || r.Feed == feedA
		sawB = sawB || r.Feed == feedB
	}
	if !sawA || !sawB {
		t.Fatalf("ListHosted(\"\", \"\") returned %d rows, missing feeds (a=%v b=%v)", len(all), sawA, sawB)
	}

	only, err := db.ListHosted(ctx, feedA, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range only {
		if r.Feed != feedA {
			t.Errorf("feed filter leaked %s", r.Feed)
		}
	}
	if len(only) != 1 {
		t.Errorf("feed filter returned %d rows, want 1", len(only))
	}
}
