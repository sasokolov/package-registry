package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/config"
	"github.com/sasokolov/package-registry/core/state"
)

// gcCmd implements `registry gc`: delete blobs no manifest points at.
//
// Mark-and-sweep with two safety rules that geo replication depends on
// (docs/geo-replication.md):
//
//   - a blob younger than -min-age is never collected, so an ingest whose
//     manifest is still being written is safe;
//   - the whole run holds a cross-replica advisory lock, so two replicas
//     cannot sweep concurrently.
//
// Dry-run is the default: deleting requires -delete.
func gcCmd(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("registry gc", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/registry/config.yaml", "path to the YAML config file")
	doDelete := flags.Bool("delete", false, "actually delete unreferenced blobs (default: dry run)")
	minAge := flags.Duration("min-age", 24*time.Hour,
		"never collect blobs younger than this (protects in-flight ingests and unreplicated peers)")
	ignoreLag := flags.Bool("ignore-replication-lag", false,
		"sweep even when a peer is behind (unsafe: a blob whose manifest has not arrived here yet looks unreferenced)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("site", cfg.Site.Name)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := api.NewStorage(cfg.Storage.Type, cfg.Storage.Options())
	if err != nil {
		return err
	}
	if init, ok := store.(api.Initializer); ok {
		if err := init.Init(ctx); err != nil {
			return err
		}
	}

	if cfg.Database.DSN == "" {
		logger.Warn("no database configured: sweeping from the blob store alone, without the cross-replica lock")
		return sweep(ctx, store, nil, out, logger, *doDelete, *minAge)
	}
	db, err := state.Open(ctx, cfg.Database.DSN, logger)
	if err != nil {
		return err
	}
	defer db.Close()
	// A site that is behind cannot tell "unreferenced" from "the manifest
	// has not arrived yet". Refuse rather than delete on a guess.
	if cfg.Replication.Enabled && !*ignoreLag {
		behind, detail, err := replicationBehind(ctx, db, cfg)
		if err != nil {
			return err
		}
		if behind {
			return fmt.Errorf(
				"refusing to sweep while replication is behind (%s): a blob whose manifest has not replicated here "+
					"yet looks unreferenced; wait for convergence or pass -ignore-replication-lag", detail)
		}
	}

	return db.WithLock(ctx, "gc", func(ctx context.Context) error {
		return sweep(ctx, store, db, out, logger, *doDelete, *minAge)
	})
}

// sweep lists every manifest, collects the referenced digests and removes
// blobs nothing points at.
func sweep(ctx context.Context, store api.BlobStore, db *state.DB, out io.Writer,
	logger *slog.Logger, doDelete bool, minAge time.Duration) error {

	referenced := make(map[string]bool)

	// PostgreSQL is the source of truth for hosted coordinates, and it also
	// remembers the OTHER side of an unresolved cross-site conflict — a
	// blob no projection points at, which an operator may yet choose with
	// `repl resolve`. Sweeping from the blob store alone would collect it.
	if db != nil {
		rows, err := db.ListHosted(ctx, "", "")
		if err != nil {
			return fmt.Errorf("list hosted manifests: %w", err)
		}
		for _, r := range rows {
			referenced[r.SHA256] = true
		}
		// Only OPEN conflicts protect both sides: until an operator
		// decides, either digest may become canonical. Once resolved, the
		// kept digest lives in hosted_manifests and the rejected one is
		// ordinary garbage.
		conflicts, err := db.ListConflicts(ctx, true)
		if err != nil {
			return fmt.Errorf("list publish conflicts: %w", err)
		}
		for _, c := range conflicts {
			referenced[c.WinnerSHA] = true
			referenced[c.LoserSHA] = true
		}
		logger.Info("marked from the database",
			"hosted", len(rows), "open_conflicts", len(conflicts))
	} else {
		logger.Warn("no database: hosted rows and conflict losers cannot be protected")
	}
	manifests, err := store.List(ctx, "manifests/")
	if err != nil {
		return fmt.Errorf("list manifests: %w", err)
	}
	var manifestCount int
	for {
		info, ok := manifests.Next(ctx)
		if !ok {
			break
		}
		manifestCount++
		digest, err := manifestDigest(ctx, store, info.Key)
		if err != nil {
			// A manifest we cannot read might reference anything: refuse to
			// sweep rather than delete something still in use.
			return fmt.Errorf("read manifest %s: %w", info.Key, err)
		}
		if digest != "" {
			referenced[digest] = true
		}
	}
	if err := manifests.Err(); err != nil {
		return fmt.Errorf("walk manifests: %w", err)
	}

	blobs, err := store.List(ctx, "blobs/sha256/")
	if err != nil {
		return fmt.Errorf("list blobs: %w", err)
	}
	var scanned, kept, collected int
	var freed int64
	cutoff := time.Now().Add(-minAge)
	for {
		info, ok := blobs.Next(ctx)
		if !ok {
			break
		}
		scanned++
		digest := info.Key[strings.LastIndex(info.Key, "/")+1:]
		switch {
		case referenced[digest]:
			kept++
		case info.ModTime.After(cutoff):
			kept++
			logger.Debug("young unreferenced blob kept", "key", info.Key, "age", time.Since(info.ModTime))
		default:
			collected++
			freed += info.Size
			_, _ = fmt.Fprintf(out, "unreferenced %s (%d bytes, age %s)\n",
				info.Key, info.Size, time.Since(info.ModTime).Truncate(time.Minute))
			if doDelete {
				if err := store.Delete(ctx, info.Key); err != nil && !errors.Is(err, api.ErrNotFound) {
					return fmt.Errorf("delete %s: %w", info.Key, err)
				}
			}
		}
	}
	if err := blobs.Err(); err != nil {
		return fmt.Errorf("walk blobs: %w", err)
	}

	// Abandoned uploads. A protocol whose write spans several requests
	// stages bytes under api.StagingPrefix; a client that crashed between
	// two of them leaves objects no manifest will ever point at, so nothing
	// above would ever collect them. The same age guard applies: an upload
	// still in progress is younger than it.
	staged, stagedBytes, err := sweepStaging(ctx, out, store, cutoff, doDelete)
	if err != nil {
		return err
	}

	mode := "dry-run"
	if doDelete {
		mode = "deleted"
	}
	_, _ = fmt.Fprintf(out, "gc %s: %d manifests, %d blobs scanned, %d kept, %d unreferenced (%d bytes), %d abandoned upload chunk(s) (%d bytes)\n",
		mode, manifestCount, scanned, kept, collected, freed, staged, stagedBytes)
	logger.Info("gc finished", "mode", mode, "manifests", manifestCount,
		"blobs", scanned, "kept", kept, "collected", collected, "bytes", freed,
		"abandoned_upload_chunks", staged, "abandoned_upload_bytes", stagedBytes)
	return nil
}

// sweepStaging collects the chunks of uploads that were never finished.
func sweepStaging(ctx context.Context, out io.Writer, store api.BlobStore,
	cutoff time.Time, doDelete bool) (int, int64, error) {
	iter, err := store.List(ctx, api.StagingPrefix)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return 0, 0, nil // nothing was ever staged
		}
		return 0, 0, fmt.Errorf("list staged uploads: %w", err)
	}
	var count int
	var bytes int64
	for {
		info, ok := iter.Next(ctx)
		if !ok {
			break
		}
		if info.ModTime.After(cutoff) {
			continue // an upload still in progress
		}
		count++
		bytes += info.Size
		_, _ = fmt.Fprintf(out, "abandoned upload chunk %s (%d bytes, age %s)\n",
			info.Key, info.Size, time.Since(info.ModTime).Truncate(time.Minute))
		if doDelete {
			if err := store.Delete(ctx, info.Key); err != nil && !errors.Is(err, api.ErrNotFound) {
				return 0, 0, fmt.Errorf("delete %s: %w", info.Key, err)
			}
		}
	}
	if err := iter.Err(); err != nil {
		return 0, 0, fmt.Errorf("walk staged uploads: %w", err)
	}
	return count, bytes, nil
}

// manifestDigest reads the blob digest a manifest object points at.
func manifestDigest(ctx context.Context, store api.BlobStore, key string) (string, error) {
	rc, _, err := store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return "", nil // raced with a delete
		}
		return "", err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return "", err
	}
	return parseManifestSHA(raw)
}

// parseManifestSHA extracts the sha256 field without pulling the pipeline's
// private manifest type into the CLI.
func parseManifestSHA(raw []byte) (string, error) {
	var doc struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	return doc.SHA256, nil
}

// replicationBehind reports whether any peer stream is unconverged or any
// event is parked — both mean this site does not yet know every manifest
// the mesh has, and a sweep would treat those blobs as garbage.
func replicationBehind(ctx context.Context, db *state.DB, cfg *config.Config) (bool, string, error) {
	parked, err := db.CountParked(ctx)
	if err != nil {
		return false, "", err
	}
	if parked > 0 {
		return true, fmt.Sprintf("%d parked event(s)", parked), nil
	}
	cursors, err := db.ListCursors(ctx)
	if err != nil {
		return false, "", err
	}
	byPeer := map[string]bool{}
	for _, c := range cursors {
		if c.LastError != "" {
			return true, fmt.Sprintf("peer %s: %s", c.Peer, c.LastError), nil
		}
		if c.AppliedSeq > c.DurableSeq {
			return true, fmt.Sprintf("peer %s has %d event(s) whose blobs are not local",
				c.Peer, c.AppliedSeq-c.DurableSeq), nil
		}
		byPeer[c.Peer] = true
	}
	for _, p := range cfg.Replication.Peers {
		if !byPeer[p.Name] {
			return true, fmt.Sprintf("peer %s has never been reached", p.Name), nil
		}
	}
	return false, "", nil
}
