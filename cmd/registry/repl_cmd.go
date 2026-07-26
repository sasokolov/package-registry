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
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/sasokolov/package-registry/core/config"
	"github.com/sasokolov/package-registry/core/repl"
	"github.com/sasokolov/package-registry/core/state"
)

// replCmd implements `registry repl <subcommand>`: the operator surface of
// geo replication (docs/geo-replication.md).
func replCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: registry repl status|conflicts|resolve|retry-parked [-config <path>]")
	}
	sub := args[0]
	flags := flag.NewFlagSet("registry repl "+sub, flag.ContinueOnError)
	configPath := flags.String("config", "/etc/registry/config.yaml", "path to the YAML config file")
	feed := flags.String("feed", "", "feed name (resolve)")
	path := flags.String("path", "", "coordinate path (resolve)")
	keep := flags.String("keep", "", "sha256 to keep (resolve)")
	openOnly := flags.Bool("open", true, "list only unresolved conflicts")
	asJSON := flags.Bool("json", false, "machine-readable output with full digests (conflicts)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Database.DSN == "" {
		return errors.New("replication commands need a database")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	db, err := state.Open(ctx, cfg.Database.DSN, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	switch sub {
	case "status":
		return replStatus(ctx, db, cfg, out)
	case "conflicts":
		return replConflicts(ctx, db, out, *openOnly, *asJSON)
	case "resolve":
		return replResolve(ctx, db, cfg, out, *feed, *path, *keep)
	case "retry-parked":
		return replRetryParked(ctx, db, out)
	default:
		return fmt.Errorf("unknown repl subcommand %q", sub)
	}
}

func replStatus(ctx context.Context, db *state.DB, cfg *config.Config, out io.Writer) error {
	identity, err := db.EnsureSiteIdentity(ctx, cfg.Site.Name)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "site %s (%s)\n", identity.Site, identity.UUID)

	origins, err := db.KnownOrigins(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "\nORIGIN\tHEAD\tOLDEST")
	for _, origin := range origins {
		head, oldest, err := db.JournalHead(ctx, origin)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\n", origin, head, oldest)
	}

	cursors, err := db.ListCursors(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(w, "\nPEER\tORIGIN\tAPPLIED\tDURABLE\tLAST OK\tLAST ERROR")
	for _, c := range cursors {
		lastOK := "never"
		if !c.LastOKAt.IsZero() {
			lastOK = time.Since(c.LastOKAt).Truncate(time.Second).String() + " ago"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n",
			c.Peer, c.Origin, c.AppliedSeq, c.DurableSeq, lastOK, truncate(c.LastError, 40))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	parked, err := db.CountParked(ctx)
	if err != nil {
		return err
	}
	conflicts, err := db.ListConflicts(ctx, true)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\nparked events: %d\nopen conflicts: %d\n", parked, len(conflicts))
	if len(conflicts) > 0 {
		_, _ = fmt.Fprintln(out, "run `registry repl conflicts` for details")
	}
	return nil
}

func replConflicts(ctx context.Context, db *state.DB, out io.Writer, openOnly, asJSON bool) error {
	rows, err := db.ListConflicts(ctx, openOnly)
	if err != nil {
		return err
	}
	if asJSON {
		// The table truncates digests for readability; automation and the
		// resolve command need them in full.
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "no conflicts")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "FEED\tPATH\tCANONICAL\tOTHER\tSITES\tDETECTED\tSTATE")
	for _, c := range rows {
		stateStr := "open"
		if c.Resolved {
			stateStr = "resolved:" + truncate(c.ResolvedSHA, 12)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s/%s\t%s\t%s\n",
			c.Feed, c.Path, truncate(c.WinnerSHA, 12), truncate(c.LoserSHA, 12),
			c.WinnerSite, c.LoserSite, c.DetectedAt.Format(time.RFC3339), stateStr)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "\nresolve with: registry repl resolve -feed <feed> -path <path> -keep <sha256>")
	return nil
}

// replResolve records an operator's decision and journals it, so every site
// converges on the same choice.
func replResolve(ctx context.Context, db *state.DB, cfg *config.Config, out io.Writer,
	feed, path, keep string) error {
	if feed == "" || path == "" || keep == "" {
		return errors.New("resolve needs -feed, -path and -keep <sha256>")
	}
	if len(keep) != 64 {
		return errors.New("-keep must be a full sha256 hex digest")
	}

	conflicts, err := db.ListConflicts(ctx, true)
	if err != nil {
		return err
	}
	var target *state.ConflictRow
	for i := range conflicts {
		if conflicts[i].Feed == feed && conflicts[i].Path == path {
			target = &conflicts[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("no open conflict for %s %s", feed, path)
	}
	if keep != target.WinnerSHA && keep != target.LoserSHA {
		return fmt.Errorf("-keep %s is neither side of the conflict (%s or %s)",
			truncate(keep, 12), truncate(target.WinnerSHA, 12), truncate(target.LoserSHA, 12))
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		"UPDATE hosted_manifests SET sha256=$3, updated_at=now() WHERE feed=$1 AND path=$2",
		feed, path, keep); err != nil {
		return fmt.Errorf("apply resolution locally: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"UPDATE quarantine SET released_at = now() WHERE feed=$1 AND coordinate=$2 AND reason='cross_site_conflict' AND released_at IS NULL",
		feed, target.Coordinate); err != nil {
		return fmt.Errorf("release quarantine: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"UPDATE publish_conflicts SET resolved_at=now(), resolved_sha256=$3 WHERE feed=$1 AND path=$2 AND resolved_at IS NULL",
		feed, path, keep); err != nil {
		return fmt.Errorf("close conflict: %w", err)
	}

	operator := os.Getenv("USER")
	if operator == "" {
		operator = "operator"
	}
	writer := repl.NewWriter(cfg.Site.Name)
	if err := writer.AppendConflictResolve(ctx, tx, feed, path, target.Coordinate, keep, operator); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit resolution: %w", err)
	}

	_, _ = fmt.Fprintf(out, "resolved %s %s -> %s (announced to peers)\n", feed, path, truncate(keep, 12))
	return nil
}

func replRetryParked(ctx context.Context, db *state.DB, out io.Writer) error {
	entries, reasons, err := db.ParkedEvents(ctx, 1000)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(out, "no parked events")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ORIGIN\tSEQ\tKIND\tREASON")
	for i, e := range entries {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", e.OriginSite, e.OriginSeq, e.Kind, truncate(reasons[i], 60))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "\nparked events are retried automatically on every poll cycle")
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
