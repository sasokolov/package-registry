//go:build integration

package repl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/sasokolov/package-registry/core/state"
)

// A site can reach the same state two ways: by replaying the journal, or by
// bootstrapping from a peer's snapshot. Nothing tested the second path, and
// that is how a resolution arrived without its size and got served as an
// empty 200.
//
// The receiving site gets its OWN database. Sharing one would share
// hlc_state, which is exactly the difference that hid a bootstrapped site
// stamping its later writes below the state it had just imported.

// freshSiteDB creates and migrates a separate database, so the importing
// site has its own clock, journal and cursors.
func freshSiteDB(ctx context.Context, t *testing.T) *state.DB {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; run via make test-integration")
	}
	name := fmt.Sprintf("fresh_site_%d", time.Now().UnixNano())

	admin, err := state.Open(ctx, dsn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Pool().Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	admin.Close()

	freshDSN := replaceDatabase(dsn, name)
	db, err := state.Open(ctx, freshDSN, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanup, err := state.Open(context.Background(), dsn, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Pool().Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return db
}

// replaceDatabase swaps the database name in a postgres DSN.
func replaceDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

// buildSnapshot mirrors handleSnapshot: the same reads, in one transaction.
func buildSnapshot(ctx context.Context, t *testing.T, db *state.DB, site, uuid string) SnapshotResponse {
	t.Helper()
	tx, err := db.BeginSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	snap := SnapshotResponse{Site: site, UUID: uuid, Watermarks: map[string]int64{}}

	rows, err := state.ListHostedTx(ctx, tx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		snap.Manifests = append(snap.Manifests, ManifestPut{
			Feed: row.Feed, Path: row.Path, Coord: row.Coordinate,
			SHA256: row.SHA256, Size: row.Size,
			Checksums: row.Checksums, Metadata: row.Metadata,
			Mutable: row.Mutable, Publisher: row.PublishedBy,
		})
	}

	quarantines, err := state.QuarantineStateTx(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range quarantines {
		snap.Quarantine = append(snap.Quarantine, QuarantineRecord{
			Feed: q.Feed, Coordinate: q.Coordinate, Reason: q.Reason,
			Detail: q.Detail, Active: q.Active,
			HLC: state.HLC{Wall: q.HLCWall, Logical: q.HLCLogical},
		})
	}

	conflicts, err := state.OpenConflictsTx(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conflicts {
		snap.Conflicts = append(snap.Conflicts, ConflictRecord{
			Feed: c.Feed, Path: c.Path, Coordinate: c.Coordinate,
			WinnerSHA: c.WinnerSHA, LoserSHA: c.LoserSHA,
			WinnerSite: c.WinnerSite, LoserSite: c.LoserSite,
			WinnerSize: c.WinnerSize, LoserSize: c.LoserSize,
			WinnerSums: c.WinnerSums, LoserSums: c.LoserSums,
			WinnerMeta: c.WinnerMeta, LoserMeta: c.LoserMeta,
		})
	}

	resolutions, err := state.ConflictResolutionsTx(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resolutions {
		snap.Resolutions = append(snap.Resolutions, ResolutionRecord{
			Feed: r.Feed, Path: r.Path, Coordinate: r.Coordinate,
			KeepSHA: r.KeepSHA, Size: r.Size,
			Checksums: r.Checksums, Metadata: r.Metadata,
			Operator: r.Operator,
			HLC:      state.HLC{Wall: r.HLCWall, Logical: r.HLCLogical},
		})
	}

	snap.Revoked, err = state.RevokedTokenHashesTx(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// applySnapshot mirrors Manager.bootstrap without the HTTP client.
func applySnapshot(ctx context.Context, t *testing.T, db *state.DB, snap SnapshotResponse) {
	t.Helper()
	applier := NewApplier(ApplierOptions{
		DB: db, Site: "fresh-site-under-test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Eager:  func(string) bool { return false },
	})

	m := &Manager{db: db, site: "fresh-site-under-test", applier: applier,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := m.applySnapshotRestrictions(ctx, snap.Site, snap); err != nil {
		t.Fatalf("apply snapshot restrictions: %v", err)
	}

	touched := map[string]bool{}
	for _, p := range snap.Manifests {
		entry := state.JournalEntry{
			OriginSite: snap.Site, Kind: KindManifestPut,
			SchemaVersion: SchemaVersion, HLC: hlcFromMetadata(p.Metadata),
		}
		payload, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		entry.Payload = payload

		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var pending []projectionWrite
		if err := applier.importSnapshotEntry(ctx, tx, snap.Site, entry, touched, &pending); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("import snapshot entry %s/%s: %v", p.Feed, p.Path, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Mirror the last thing bootstrap does: adopt the snapshot's clock.
	if err := m.advanceClockPastSnapshot(ctx, snap); err != nil {
		t.Fatalf("advance clock: %v", err)
	}
}

// TestBootstrapReachesTheSameStateAsTheJournal is the cross-check between
// the two ways a site can converge.
func TestBootstrapReachesTheSameStateAsTheJournal(t *testing.T) {
	db := integrationDB(t)
	ctx := t.Context()

	for seed := uint64(1); seed <= 5; seed++ {
		sourceFeed := fmt.Sprintf("snap-src-%d-%d", seed, time.Now().UnixNano())
		rng := rand.New(rand.NewPCG(seed, seed*11+5))
		events := randomEventsForFeed(t, rng, sourceFeed)

		// Build the reference state through the journal.
		resetReplicationState(ctx, t, db, sourceFeed)
		applyAll(ctx, t, db, events)
		reference := applierFingerprint(ctx, t, db, sourceFeed)

		// Snapshot it exactly as a peer would serve it.
		snap := buildSnapshot(ctx, t, db, "source-site", "11111111-1111-1111-1111-111111111111")

		// Import into a site with its own database and its own clock.
		fresh := freshSiteDB(ctx, t)
		applySnapshot(ctx, t, fresh, snap)
		bootstrapped := applierFingerprint(ctx, t, fresh, sourceFeed)

		if bootstrapped != reference {
			t.Fatalf("seed %d: bootstrap and journal disagree\n--- journal ---\n%s\n--- bootstrap ---\n%s",
				seed, reference, bootstrapped)
		}
	}
}

// A resolved coordinate must arrive with its size and checksums: without
// them the coordinate is stored as zero-length and served as an empty 200,
// with every digest still matching.
func TestBootstrapCarriesResolutionSizes(t *testing.T) {
	db := integrationDB(t)
	ctx := t.Context()
	feed := fmt.Sprintf("snap-size-%d", time.Now().UnixNano())
	resetReplicationState(ctx, t, db, feed)

	path := "lib/1.0.0/lib.jar"
	coord := "maven:com.example:lib@1.0.0"
	a, b := digestOf("side a"), digestOf("side b")

	// Two sites publish different bytes, K1 records the conflict, an
	// operator resolves it — the journal way.
	events := []state.JournalEntry{
		mkEvent(t, "eu-1", 1, 1000, 0, KindManifestPut, ManifestPut{
			Feed: feed, Path: path, Coord: coord, SHA256: a, Size: 6,
			Checksums: map[string]string{"sha1": "aaa"}}),
		mkEvent(t, "us-1", 1, 1001, 0, KindManifestPut, ManifestPut{
			Feed: feed, Path: path, Coord: coord, SHA256: b, Size: 6,
			Checksums: map[string]string{"sha1": "bbb"}}),
		mkEvent(t, "eu-1", 2, 2000, 0, KindConflictResolve, ConflictResolve{
			Feed: feed, Path: path, Coord: coord, KeepSHA: b, Operator: "alice"}),
	}
	applyAll(ctx, t, db, events)

	var size int64
	var checksums string
	if err := db.Pool().QueryRow(ctx,
		"SELECT size, checksums::text FROM hosted_manifests WHERE feed=$1 AND path=$2",
		feed, path).Scan(&size, &checksums); err != nil {
		t.Fatal(err)
	}
	if size == 0 {
		t.Fatal("the journal path itself stored a zero size")
	}

	snap := buildSnapshot(ctx, t, db, "source-site", "22222222-2222-2222-2222-222222222222")

	fresh := freshSiteDB(ctx, t)
	applySnapshot(ctx, t, fresh, snap)

	var gotSize int64
	var gotSHA, gotChecksums string
	if err := fresh.Pool().QueryRow(ctx,
		"SELECT sha256, size, checksums::text FROM hosted_manifests WHERE feed=$1 AND path=$2",
		feed, path).Scan(&gotSHA, &gotSize, &gotChecksums); err != nil {
		t.Fatalf("the bootstrapped site has no row for the resolved coordinate: %v", err)
	}
	if gotSHA != b {
		t.Errorf("bootstrapped digest = %s, want the operator's choice %s", gotSHA, b)
	}
	if gotSize != size {
		t.Errorf("bootstrapped size = %d, want %d: a zero-length serve with a matching digest",
			gotSize, size)
	}
	if gotChecksums != checksums {
		t.Errorf("bootstrapped checksums = %s, want %s", gotChecksums, checksums)
	}
}

// A bootstrapped site must stamp its OWN later writes after the state it
// imported. Without that the site keeps its write, every peer drops it as
// older, and nothing reports the divergence.
func TestBootstrapAdvancesTheLocalClock(t *testing.T) {
	db := integrationDB(t)
	ctx := t.Context()
	feed := fmt.Sprintf("snap-clock-%d", time.Now().UnixNano())
	resetReplicationState(ctx, t, db, feed)

	// A coordinate stamped well into the future, the way a peer whose clock
	// legitimately ran ahead (within max_clock_skew) would produce.
	future := time.Now().Add(4 * time.Minute).UnixMilli()
	path := "-/hosted/pkg/dist-tags/latest"
	events := []state.JournalEntry{
		mkEvent(t, "eu-1", 1, future, 0, KindManifestPut, ManifestPut{
			Feed: feed, Path: path, Coord: "npm:pkg",
			SHA256: digestOf("v1"), Size: 2, Mutable: true,
		}),
	}
	applyAll(ctx, t, db, events)

	snap := buildSnapshot(ctx, t, db, "source-site", "33333333-3333-3333-3333-333333333333")
	fresh := freshSiteDB(ctx, t)
	applySnapshot(ctx, t, fresh, snap)

	// The importing site's own clock must now be past the imported stamp.
	var wall int64
	if err := fresh.Pool().QueryRow(ctx,
		"SELECT wall_ms FROM hlc_state WHERE id").Scan(&wall); err != nil {
		t.Fatal(err)
	}
	if wall < future {
		t.Fatalf("the bootstrapped site's clock is %d, behind the imported stamp %d: "+
			"its next write would be ordered before the state it just adopted", wall, future)
	}

	// And a local write really does win against the imported state.
	local, err := fresh.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.InsertHostedTx(ctx, local, state.HostedRow{
		Feed: feed, Path: path, Coordinate: "npm:pkg",
		SHA256: digestOf("v2"), Size: 2, Mutable: true, Origin: "publish", Site: "fresh",
	}); err != nil {
		_ = local.Rollback(ctx)
		t.Fatal(err)
	}
	if err := local.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Replaying the imported event must not undo it.
	applyAll(ctx, t, fresh, events)
	var sha string
	if err := fresh.Pool().QueryRow(ctx,
		"SELECT sha256 FROM hosted_manifests WHERE feed=$1 AND path=$2", feed, path).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	if sha != digestOf("v2") {
		t.Error("a write made after the bootstrap lost to the state the bootstrap imported")
	}
}
