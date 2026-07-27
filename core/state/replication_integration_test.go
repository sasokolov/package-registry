//go:build integration

package state

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The journal's core guarantee: a reader paginating by origin_seq can never
// skip an entry. That only holds if sequence order equals commit order,
// which is why repl_hlc_next() allocates the sequence under the hlc_state
// row lock. This test hammers it with concurrent writers while a reader
// pages through, and fails if anything is missed.
func TestJournalSequenceOrderMatchesCommitOrder(t *testing.T) {
	dsn := os.Getenv("REGISTRY_TEST_DSN")
	if dsn == "" {
		t.Skip("REGISTRY_TEST_DSN not set")
	}
	ctx := t.Context()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	db, err := Open(ctx, dsn, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	site := fmt.Sprintf("seq-test-%d", time.Now().UnixNano())

	const (
		writers        = 8
		perWriter      = 40
		expectedWrites = writers * perWriter
	)

	var written atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < perWriter; i++ {
				tx, err := db.Begin(ctx)
				if err != nil {
					t.Errorf("begin: %v", err)
					return
				}
				_, err = AppendJournal(ctx, tx, site, "manifest_put",
					map[string]any{"writer": w, "index": i})
				if err != nil {
					_ = tx.Rollback(ctx)
					t.Errorf("append: %v", err)
					return
				}
				if err := tx.Commit(ctx); err != nil {
					t.Errorf("commit: %v", err)
					return
				}
				written.Add(1)
			}
		}(w)
	}

	// The reader pages exactly the way a peer does: read after a cursor,
	// advance the cursor to the last sequence it saw, repeat.
	readDone := make(chan map[int64]bool, 1)
	go func() {
		seen := map[int64]bool{}
		var cursor int64
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			entries, err := db.ReadJournal(ctx, site, cursor, 50)
			if err != nil {
				t.Errorf("read journal: %v", err)
				break
			}
			for _, e := range entries {
				seen[e.OriginSeq] = true
				cursor = e.OriginSeq
			}
			if len(seen) >= expectedWrites && written.Load() == expectedWrites {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		readDone <- seen
	}()

	close(start)
	wg.Wait()
	seen := <-readDone

	// Give the reader a last pass for anything committed after it stopped.
	var cursor int64
	for seq := range seen {
		if seq > cursor {
			cursor = seq
		}
	}
	tail, err := db.ReadJournal(ctx, site, cursor, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tail {
		seen[e.OriginSeq] = true
	}

	head, _, err := db.JournalHead(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	if head != expectedWrites {
		t.Fatalf("head = %d, want %d (sequences were skipped or duplicated)", head, expectedWrites)
	}
	var missing []int64
	for seq := int64(1); seq <= head; seq++ {
		if !seen[seq] {
			missing = append(missing, seq)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the paginating reader missed %d entries (first few: %v): "+
			"sequence order does not match commit order", len(missing), missing[:min(5, len(missing))])
	}
}

// Quarantine reasons are independent: releasing one must not lift another,
// which is what makes the merge rules commutative.
func TestQuarantineReasonsAreIndependent(t *testing.T) {
	dsn := os.Getenv("REGISTRY_TEST_DSN")
	if dsn == "" {
		t.Skip("REGISTRY_TEST_DSN not set")
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	db, err := Open(ctx, dsn, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	feed := fmt.Sprintf("qfeed-%d", time.Now().UnixNano())
	coord := "maven:com.example:lib@1.0.0"
	if err := db.Quarantine(ctx, feed, coord, "manual", "takedown"); err != nil {
		t.Fatal(err)
	}
	if err := db.Quarantine(ctx, feed, coord, "cross_site_conflict", "K1"); err != nil {
		t.Fatal(err)
	}

	if err := db.ReleaseQuarantine(ctx, feed, coord, "cross_site_conflict"); err != nil {
		t.Fatal(err)
	}
	entry, active, err := db.ActiveQuarantine(ctx, feed, coord)
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("releasing the conflict also lifted the manual takedown")
	}
	if entry.Reason != "manual" {
		t.Errorf("remaining reason = %q, want manual", entry.Reason)
	}

	if err := db.ReleaseQuarantine(ctx, feed, coord, ""); err != nil {
		t.Fatal(err)
	}
	if _, active, err = db.ActiveQuarantine(ctx, feed, coord); err != nil || active {
		t.Errorf("coordinate still quarantined after releasing every reason (err=%v)", err)
	}
}

// TryLease is what stops the N stateless replicas of a site from all
// pulling the same peer stream. Nothing exercised it: the geo stack runs
// one registry per site and the chaos stack has no replication.
func TestTryLeaseAdmitsExactlyOneHolder(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; run via make test-integration")
	}
	ctx := t.Context()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	db, err := Open(ctx, dsn, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	key := fmt.Sprintf("lease-test-%d", time.Now().UnixNano())

	// One holder is inside the critical section; a second attempt must be
	// turned away rather than queued, so a slow poll never stacks up.
	inside := make(chan struct{})
	release := make(chan struct{})
	held := make(chan error, 1)
	go func() {
		_, err := db.TryLease(ctx, key, func(context.Context) error {
			close(inside)
			<-release
			return nil
		})
		held <- err
	}()

	<-inside
	ran, err := db.TryLease(ctx, key, func(context.Context) error {
		t.Error("a second holder entered the critical section")
		return nil
	})
	if err != nil {
		t.Fatalf("second TryLease: %v", err)
	}
	if ran {
		t.Error("TryLease admitted a second holder")
	}

	close(release)
	if err := <-held; err != nil {
		t.Fatalf("first TryLease: %v", err)
	}

	// Once released, the next caller gets in.
	ran, err = db.TryLease(ctx, key, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("the lease was not released")
	}
}

// The lease must not be lent the pool connection the protected function
// needs: with a small pool that is a deadlock, not a slowdown.
func TestLeaseDoesNotStarveTheProtectedFunction(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; run via make test-integration")
	}
	ctx := t.Context()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// A pool of one: if the lease borrowed from it, the function below
	// could never acquire a connection and this would hang.
	db, err := Open(ctx, dsn+"?pool_max_conns=1", logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	done := make(chan error, 1)
	go func() {
		_, err := db.TryLease(ctx, fmt.Sprintf("starve-%d", time.Now().UnixNano()),
			func(ctx context.Context) error {
				var one int
				return db.Pool().QueryRow(ctx, "SELECT 1").Scan(&one)
			})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("protected function failed: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the lease starved the function it protects (pool exhaustion)")
	}
}
