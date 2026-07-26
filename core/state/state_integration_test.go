//go:build integration

// Integration tests against Postgres from the compose stack:
//
//	make test-integration
//
// Required env: PG_TEST_DSN.
package state

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
)

func openDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; run via make test-integration")
	}
	db, err := Open(t.Context(), dsn, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openDB(t)
	ctx := t.Context()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	for _, table := range []string{"tokens", "audit", "publish_sessions", "quarantine"} {
		var got string
		err := db.Pool().QueryRow(ctx,
			"SELECT table_name FROM information_schema.tables WHERE table_schema='public' AND table_name=$1",
			table).Scan(&got)
		if err != nil {
			t.Errorf("table %s missing after migration: %v", table, err)
		}
	}
}

func TestWithLockSerializesSameKey(t *testing.T) {
	db := openDB(t)
	ctx := t.Context()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var inside int
	var maxInside int

	// Serialization check: track concurrent holders of one lock key.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := db.WithLock(ctx, "serialize-key", func(context.Context) error {
				mu.Lock()
				inside++
				if inside > maxInside {
					maxInside = inside
				}
				mu.Unlock()
				time.Sleep(50 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Errorf("WithLock: %v", err)
			}
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Errorf("max concurrent holders of the same lock = %d, want 1", maxInside)
	}
}

func TestWithLockDifferentKeysDoNotBlock(t *testing.T) {
	db := openDB(t)
	ctx := t.Context()

	started := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		key := string(rune('a' + i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := db.WithLock(ctx, "diff-"+key, func(context.Context) error {
				time.Sleep(200 * time.Millisecond)
				return nil
			})
			if err != nil {
				t.Errorf("WithLock: %v", err)
			}
		}()
	}
	wg.Wait()
	// Three 200ms critical sections on distinct keys must overlap.
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Errorf("distinct keys appear serialized: took %s", elapsed)
	}
}

func TestLockIDStable(t *testing.T) {
	if LockID("abc") != LockID("abc") {
		t.Error("LockID not deterministic")
	}
	if LockID("abc") == LockID("abd") {
		t.Error("LockID collides on trivially different keys")
	}
}
