//go:build integration

package repl

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/sasokolov/package-registry/core/state"
)

// The convergence property is the load-bearing claim of geo replication, and
// until now it was only ever checked against modelState — a hand-written
// mirror of the merge rules. This test runs the SAME generated event sets
// through the real Applier and a real PostgreSQL, so a rule that exists in
// the model but not in the code (or the other way round) fails here.

func integrationDB(t *testing.T) *state.DB {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; run via make test-integration")
	}
	db, err := state.Open(t.Context(), dsn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// resetReplicationState clears everything the merge rules write, so each
// permutation starts from the same blank slate.
func resetReplicationState(ctx context.Context, t *testing.T, db *state.DB) {
	t.Helper()
	for _, stmt := range []string{
		"TRUNCATE hosted_manifests",
		"TRUNCATE quarantine",
		"TRUNCATE publish_conflicts",
		"TRUNCATE conflict_resolutions",
		"TRUNCATE repl_journal",
		"TRUNCATE repl_parked",
		"TRUNCATE repl_cursors",
		"DELETE FROM tokens WHERE name LIKE 'converge-%'",
	} {
		if _, err := db.Pool().Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// The generator revokes these; they must exist for a revocation to be
	// observable.
	for i := 0; i < 3; i++ {
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO tokens (name, hash) VALUES ($1, $2)
			ON CONFLICT (name) DO NOTHING`,
			fmt.Sprintf("converge-token-%d", i), digestOf(fmt.Sprintf("token-%d", i))); err != nil {
			t.Fatalf("seed token: %v", err)
		}
	}
}

// applierFingerprint renders every piece of replicated state the merge rules
// decide, so two runs can be compared exactly.
func applierFingerprint(ctx context.Context, t *testing.T, db *state.DB) string {
	t.Helper()
	var b strings.Builder

	rows, err := db.ListHosted(ctx, "", "")
	if err != nil {
		t.Fatalf("list hosted: %v", err)
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "manifest|%s/%s=%s\n", r.Feed, r.Path, r.SHA256)
	}

	// Active quarantines, by coordinate and reason.
	qrows, err := db.Pool().Query(ctx, `
		SELECT feed, coordinate, reason FROM quarantine
		 WHERE released_at IS NULL ORDER BY feed, coordinate, reason`)
	if err != nil {
		t.Fatalf("read quarantine: %v", err)
	}
	for qrows.Next() {
		var feed, coord, reason string
		if err := qrows.Scan(&feed, &coord, &reason); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "quarantine|%s/%s=%s\n", feed, coord, reason)
	}
	qrows.Close()

	// The SET of digests ever recorded per path (a union, so it must not
	// depend on arrival order).
	crows, err := db.Pool().Query(ctx, `
		SELECT feed, path, winner_sha256, loser_sha256 FROM publish_conflicts`)
	if err != nil {
		t.Fatalf("read conflicts: %v", err)
	}
	seen := map[string]map[string]bool{}
	for crows.Next() {
		var feed, path, winner, loser string
		if err := crows.Scan(&feed, &path, &winner, &loser); err != nil {
			t.Fatal(err)
		}
		key := feed + "/" + path
		if seen[key] == nil {
			seen[key] = map[string]bool{}
		}
		seen[key][winner] = true
		seen[key][loser] = true
	}
	crows.Close()
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		digests := make([]string, 0, len(seen[k]))
		for d := range seen[k] {
			digests = append(digests, d)
		}
		sort.Strings(digests)
		fmt.Fprintf(&b, "conflict|%s=%s\n", k, strings.Join(digests, ","))
	}

	rrows, err := db.Pool().Query(ctx,
		"SELECT feed, path, keep_sha256 FROM conflict_resolutions ORDER BY feed, path")
	if err != nil {
		t.Fatalf("read resolutions: %v", err)
	}
	for rrows.Next() {
		var feed, path, keep string
		if err := rrows.Scan(&feed, &path, &keep); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "resolved|%s/%s=%s\n", feed, path, keep)
	}
	rrows.Close()

	trows, err := db.Pool().Query(ctx,
		"SELECT hash FROM tokens WHERE revoked_at IS NOT NULL ORDER BY hash")
	if err != nil {
		t.Fatalf("read revocations: %v", err)
	}
	for trows.Next() {
		var h string
		if err := trows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "revoked|%s\n", h)
	}
	trows.Close()

	return b.String()
}

// applyAll feeds the events through the real applier, retrying parked ones
// after every event exactly as the puller does on each poll cycle.
func applyAll(ctx context.Context, t *testing.T, db *state.DB, events []state.JournalEntry) {
	t.Helper()
	applier := NewApplier(ApplierOptions{
		DB:     db,
		Site:   "local-under-test", // no generated event originates here
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Eager:  func(string) bool { return false },
	})
	touched := map[string]bool{}
	for _, e := range events {
		if err := applier.applyOne(ctx, e.OriginSite, e, touched); err != nil {
			t.Fatalf("apply %s/%d (%s): %v", e.OriginSite, e.OriginSeq, e.Kind, err)
		}
		if err := applier.RetryParked(ctx, e.OriginSite); err != nil {
			t.Fatalf("retry parked: %v", err)
		}
	}
	// A few extra passes: a parked event may only become applicable after
	// several others have landed.
	for i := 0; i < 3; i++ {
		if err := applier.RetryParked(ctx, "retry"); err != nil {
			t.Fatalf("retry parked: %v", err)
		}
	}
}

// TestApplierConvergesUnderAnyOrder is the real-code counterpart of
// TestMergeConvergesUnderAnyOrder.
func TestApplierConvergesUnderAnyOrder(t *testing.T) {
	db := integrationDB(t)
	ctx := t.Context()

	for seed := uint64(1); seed <= 8; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*31+7))
		events := randomEvents(t, rng)

		resetReplicationState(ctx, t, db)
		applyAll(ctx, t, db, events)
		want := applierFingerprint(ctx, t, db)

		for trial := 0; trial < 3; trial++ {
			shuffled := append([]state.JournalEntry(nil), events...)
			rng.Shuffle(len(shuffled), func(i, j int) {
				shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
			})
			// Delivery is at-least-once.
			for k := 0; k < 3 && len(shuffled) > 0; k++ {
				idx := rng.IntN(len(shuffled))
				shuffled = append(shuffled, shuffled[idx])
			}

			resetReplicationState(ctx, t, db)
			applyAll(ctx, t, db, shuffled)
			got := applierFingerprint(ctx, t, db)

			if got != want {
				t.Fatalf("seed %d trial %d: the applier diverged\n--- reference ---\n%s\n--- shuffled ---\n%s",
					seed, trial, want, got)
			}
		}
	}
}

// TestApplierMatchesTheModel pins the model honest: the hand-written mirror
// used by the fast unit test must agree with the implementation, or the
// unit test is proving nothing about the code.
func TestApplierMatchesTheModel(t *testing.T) {
	db := integrationDB(t)
	ctx := t.Context()

	for seed := uint64(1); seed <= 8; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*17+3))
		events := randomEvents(t, rng)

		resetReplicationState(ctx, t, db)
		applyAll(ctx, t, db, events)

		model := newModel()
		for _, e := range events {
			model.apply(e, "local-under-test")
		}

		real := normalizeFingerprint(applierFingerprint(ctx, t, db))
		want := normalizeFingerprint(model.fingerprint())
		if real != want {
			t.Fatalf("seed %d: the model and the implementation disagree\n--- model ---\n%s\n--- implementation ---\n%s",
				seed, want, real)
		}
	}
}

// normalizeFingerprint puts both representations in the same shape: the
// model joins a coordinate's quarantine reasons on one line, the database
// yields a row each. Only the formatting differs, so it is normalized away
// rather than papered over — the values themselves are compared exactly.
func normalizeFingerprint(s string) string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l == "" {
			continue
		}
		if key, values, ok := strings.Cut(l, "="); ok && strings.Contains(values, ",") &&
			(strings.HasPrefix(l, "quarantine|") || strings.HasPrefix(l, "conflict|")) {
			for _, v := range strings.Split(values, ",") {
				out = append(out, key+"="+v)
			}
			continue
		}
		out = append(out, l)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}
