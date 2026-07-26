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
		conflicts, err := db.ListConflicts(ctx, false)
		if err != nil {
			return fmt.Errorf("list publish conflicts: %w", err)
		}
		for _, c := range conflicts {
			referenced[c.WinnerSHA] = true
			referenced[c.LoserSHA] = true
		}
		logger.Info("marked from the database",
			"hosted", len(rows), "conflict_sides", 2*len(conflicts))
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

	mode := "dry-run"
	if doDelete {
		mode = "deleted"
	}
	_, _ = fmt.Fprintf(out, "gc %s: %d manifests, %d blobs scanned, %d kept, %d unreferenced (%d bytes)\n",
		mode, manifestCount, scanned, kept, collected, freed)
	logger.Info("gc finished", "mode", mode, "manifests", manifestCount,
		"blobs", scanned, "kept", kept, "collected", collected, "bytes", freed)
	return nil
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
