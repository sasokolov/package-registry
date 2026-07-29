//go:build integration

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/core/state"
	"github.com/fondaco-dev/fondaco/modules/storage/fs"
)

// The two rules that keep gc from deleting live data are database-backed:
// a hosted row whose projection is missing, and the losing side of an OPEN
// cross-site conflict, which an operator may still choose. The unit tests
// pass db=nil, so neither had ever run.
func TestGCProtectsDatabaseReferences(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; run via make test-integration")
	}
	ctx := t.Context()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := state.Open(ctx, dsn, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	store, err := fs.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	feed := fmt.Sprintf("gc-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Pool().Exec(ctx, "DELETE FROM hosted_manifests WHERE feed=$1", feed)
		_, _ = db.Pool().Exec(ctx, "DELETE FROM publish_conflicts WHERE feed=$1", feed)
	})

	put := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		digest := hex.EncodeToString(sum[:])
		if err := store.Put(ctx, "blobs/sha256/"+digest,
			strings.NewReader(content), api.PutOpts{SHA256: digest}); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-72 * time.Hour)
		if err := os.Chtimes(filepath.Join(dir, "data", "blobs", "sha256", digest), old, old); err != nil {
			t.Fatal(err)
		}
		return digest
	}

	// A published coordinate whose blob-store projection never landed (an
	// S3 blip during publish). The database is the source of truth.
	rowOnly := put("referenced only by a database row")
	if _, err := db.InsertHosted(ctx, state.HostedRow{
		Feed: feed, Path: "lib/1.0.0/lib.jar", Coordinate: "maven:lib@1.0.0",
		SHA256: rowOnly, Size: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Both sides of an OPEN conflict: the loser is what `repl resolve
	// -keep <loser>` would restore, so it must survive.
	winner := put("the canonical side")
	loser := put("the side an operator may still choose")
	if err := db.RecordConflict(ctx, feed, "lib/2.0.0/lib.jar", "maven:lib@2.0.0",
		winner, loser, "eu-1", "us-1"); err != nil {
		t.Fatal(err)
	}

	// A blob nothing points at, from a conflict that was already resolved.
	resolvedLoser := put("rejected by an operator and no longer needed")
	if err := db.RecordConflict(ctx, feed, "lib/3.0.0/lib.jar", "maven:lib@3.0.0",
		put("kept side"), resolvedLoser, "eu-1", "us-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveConflict(ctx, feed, "lib/3.0.0/lib.jar", winner); err != nil {
		t.Fatal(err)
	}

	trueOrphan := put("nothing anywhere points at me")

	var out bytes.Buffer
	if err := sweep(ctx, store, db, &out, logger, true, 24*time.Hour); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	for name, digest := range map[string]string{
		"a hosted row whose projection is missing": rowOnly,
		"the canonical side of an open conflict":   winner,
		"the losing side of an OPEN conflict":      loser,
	} {
		if _, err := store.Stat(ctx, "blobs/sha256/"+digest); err != nil {
			t.Errorf("gc deleted %s: %v", name, err)
		}
	}
	if _, err := store.Stat(ctx, "blobs/sha256/"+trueOrphan); err == nil {
		t.Error("gc kept a blob nothing references")
	}
	if _, err := store.Stat(ctx, "blobs/sha256/"+resolvedLoser); err == nil {
		t.Error("gc kept the rejected side of a RESOLVED conflict, which can never be collected")
	}
}
