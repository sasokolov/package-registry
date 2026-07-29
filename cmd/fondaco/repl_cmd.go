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
	"text/tabwriter"
	"time"

	"net/http"

	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/core/config"
	"github.com/fondaco-dev/fondaco/core/pipeline"
	"github.com/fondaco-dev/fondaco/core/repl"
	"github.com/fondaco-dev/fondaco/core/state"
)

// replCmd implements `fondaco repl <subcommand>`: the operator surface of
// geo replication (docs/geo-replication.md).
func replCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(
			"usage: fondaco repl status|peers|conflicts|resolve|retry-parked|resync|backfill|trust-reset|" +
				"quarantine|release [-config <path>]")
	}
	sub := args[0]
	flags := flag.NewFlagSet("fondaco repl "+sub, flag.ContinueOnError)
	configPath := flags.String("config", "/etc/fondaco/config.yaml", "path to the YAML config file")
	feed := flags.String("feed", "", "feed name (resolve, quarantine, release)")
	path := flags.String("path", "", "coordinate path (resolve)")
	keep := flags.String("keep", "", "sha256 to keep (resolve)")
	coord := flags.String("coordinate", "", "package coordinate, e.g. maven:com.example:lib@1.0.0 (quarantine, release)")
	reason := flags.String("reason", "manual", "quarantine reason (quarantine, release)")
	detail := flags.String("detail", "", "human-readable explanation (quarantine)")
	openOnly := flags.Bool("open", true, "list only unresolved conflicts")
	peer := flags.String("peer", "", "peer name (resync, backfill)")
	dryRun := flags.Bool("dry-run", true, "report what backfill would fetch without fetching (backfill)")
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
		return replRetryParked(ctx, db, cfg, out)
	case "peers":
		return replPeers(ctx, db, cfg, out)
	case "resync":
		return replResync(ctx, db, cfg, *peer, out)
	case "backfill":
		return replBackfill(ctx, db, cfg, *peer, *dryRun, out)
	case "trust-reset":
		return replTrustReset(ctx, db, cfg, *peer, out)
	case "quarantine":
		return replQuarantine(ctx, db, cfg, *feed, *coord, *reason, *detail, out)
	case "release":
		return replRelease(ctx, db, cfg, *feed, *coord, *reason, out)
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

	identities, err := db.ListPeerIdentities(ctx)
	if err != nil {
		return err
	}
	if len(identities) > 0 {
		w2 := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w2, "\nPEER\tPINNED IDENTITY\tFIRST SEEN\tLAST SEEN")
		for _, id := range identities {
			_, _ = fmt.Fprintf(w2, "%s\t%s\t%s\t%s ago\n", id.Peer, id.UUID,
				id.FirstSeen.Format(time.RFC3339),
				time.Since(id.LastSeen).Truncate(time.Second))
		}
		if err := w2.Flush(); err != nil {
			return err
		}
	}
	// A peer whose identity stopped matching is the one failure an operator
	// must not have to dig out of a log.
	for _, c := range cursors {
		if strings.Contains(c.LastError, "pinned to") {
			_, _ = fmt.Fprintf(out,
				"\nPEER IDENTITY MISMATCH: %s — %s\n"+
					"  resolve with: fondaco repl trust-reset -peer %s (only if this really is the same site)\n",
				c.Peer, c.LastError, c.Peer)
			break
		}
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
		_, _ = fmt.Fprintln(out, "run `fondaco repl conflicts` for details")
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
	_, _ = fmt.Fprintln(out, "\nresolve with: fondaco repl resolve -feed <feed> -path <path> -keep <sha256>")
	return nil
}

// replResolve applies an operator's decision through the same code path the
// applier uses, so the local site and every peer end in identical state.
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

	// The read path serves from the blob store, so the projection has to
	// follow the decision as well.
	store, err := api.NewStorage(cfg.Storage.Type, cfg.Storage.Options())
	if err != nil {
		return err
	}
	publisher := pipeline.NewPublisher(pipeline.PublisherOptions{
		Store: store, DB: db, Site: cfg.Site.Name,
		Logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})

	operator := os.Getenv("USER")
	if operator == "" {
		operator = "operator"
	}
	if err := repl.ResolveConflict(ctx, db, publisher, cfg.Site.Name,
		feed, path, target.Coordinate, keep, operator); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "resolved %s %s -> %s (announced to peers)\n", feed, path, truncate(keep, 12))
	return nil
}

// replRetryParked lists parked events and actually retries them: the alert
// and the runbook both send an operator here to clear them, so the command
// has to do something.
func replRetryParked(ctx context.Context, db *state.DB, cfg *config.Config, out io.Writer) error {
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

	before := len(entries)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	store, err := api.NewStorage(cfg.Storage.Type, cfg.Storage.Options())
	if err != nil {
		return err
	}
	applier, manager, err := offlineApplier(ctx, db, cfg, store, logger)
	if err != nil {
		return err
	}
	applier.SetBlobs(manager)

	// Retry each origin's stream: a blob that has since arrived, a clock
	// that has caught up, or a binary that now understands the event kind.
	origins := map[string]bool{}
	for _, e := range entries {
		origins[e.OriginSite] = true
	}
	for origin := range origins {
		if err := applier.RetryParked(ctx, origin); err != nil {
			_, _ = fmt.Fprintf(out, "retrying %s: %v\n", origin, err)
		}
	}

	after, _, err := db.ParkedEvents(ctx, 1000)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\nretried: %d parked before, %d after\n", before, len(after))
	if len(after) > 0 {
		_, _ = fmt.Fprintln(out,
			"the rest still cannot be applied; the reasons above say why (they are also retried on every poll cycle)")
	}
	return nil
}

// offlineApplier builds an applier and a peer manager for one-shot CLI use.
func offlineApplier(ctx context.Context, db *state.DB, cfg *config.Config,
	store api.BlobStore, logger *slog.Logger) (*repl.Applier, *repl.Manager, error) {
	clients, err := peerClients(cfg.Replication, cfg.Site.Name,
		&http.Client{Timeout: 10 * time.Minute}, logger)
	if err != nil {
		return nil, nil, err
	}
	applier := repl.NewApplier(repl.ApplierOptions{
		DB: db, Site: cfg.Site.Name, Logger: logger,
		MaxSkew: cfg.Replication.SkewOrDefault(),
		Eager:   func(string) bool { return false },
	})
	manager := repl.NewManager(repl.ManagerOptions{
		DB: db, Store: store, Site: cfg.Site.Name,
		Clients: clients, Applier: applier, Logger: logger,
	})
	_ = ctx
	return applier, manager, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// replPeers shows the configured mesh and how each stream is doing, which
// is the first thing an operator looks at when convergence stalls.
func replPeers(ctx context.Context, db *state.DB, cfg *config.Config, out io.Writer) error {
	if !cfg.Replication.Enabled {
		_, _ = fmt.Fprintln(out, "replication is disabled in this config")
		return nil
	}
	cursors, err := db.ListCursors(ctx)
	if err != nil {
		return err
	}
	byPeer := map[string][]state.Cursor{}
	for _, c := range cursors {
		byPeer[c.Peer] = append(byPeer[c.Peer], c)
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "PEER\tURL\tINTERVAL\tSTREAMS\tLAST OK\tSTATE")
	for _, p := range cfg.Replication.Peers {
		streams := byPeer[p.Name]
		lastOK := "never"
		state := "unreachable"
		for _, c := range streams {
			if c.LastOKAt.IsZero() {
				continue
			}
			if lastOK == "never" || time.Since(c.LastOKAt) < 0 {
				lastOK = time.Since(c.LastOKAt).Truncate(time.Second).String() + " ago"
			}
			if c.LastError == "" {
				state = "ok"
			} else {
				state = "degraded"
			}
		}
		if len(streams) == 0 {
			state = "no streams yet"
		}
		interval := p.PullInterval.Std()
		if interval == 0 {
			interval = 10 * time.Second
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			p.Name, p.URL, interval, len(streams), lastOK, state)
	}
	return w.Flush()
}

// replResync rewinds a peer's cursors so the next poll re-reads the whole
// retained journal. Apply is idempotent, so this is safe to run at any
// time; it is the fix for "this site missed something".
func replResync(ctx context.Context, db *state.DB, cfg *config.Config, peer string, out io.Writer) error {
	if peer == "" {
		return errors.New("resync needs -peer <name>")
	}
	var reset, dropped int64
	// Take the poll lease so a cycle that is already running cannot write a
	// higher cursor straight back over the reset.
	ran, err := db.TryLease(ctx, "repl-poll:"+cfg.Site.Name+":"+peer, func(ctx context.Context) error {
		// Drop this site's copy of the peer's journal as well: apply
		// deduplicates on (origin, seq), so rewinding the cursor alone
		// would re-read the entries and skip every one of them.
		var err error
		reset, dropped, err = db.ResetPeerStream(ctx, peer)
		return err
	})
	if err != nil {
		return err
	}
	if !ran {
		return fmt.Errorf("a poll cycle for peer %q is running; try again in a moment", peer)
	}
	if reset == 0 {
		return fmt.Errorf("no cursors found for peer %q", peer)
	}
	_, _ = fmt.Fprintf(out,
		"reset %d cursor(s) and dropped %d journal entr(ies) for peer %s; the next poll re-reads and re-applies\n",
		reset, dropped, peer)
	return nil
}

// replBackfill reports (and optionally repairs) hosted coordinates whose
// blob is missing locally — the "manifest here, bytes elsewhere" state that
// lazy feeds live in until something asks for them.
func replBackfill(ctx context.Context, db *state.DB, cfg *config.Config,
	peer string, dryRun bool, out io.Writer) error {
	store, err := api.NewStorage(cfg.Storage.Type, cfg.Storage.Options())
	if err != nil {
		return err
	}
	if init, ok := store.(api.Initializer); ok {
		if err := init.Init(ctx); err != nil {
			return fmt.Errorf("initialize storage: %w", err)
		}
	}
	rows, err := db.ListHosted(ctx, "", "")
	if err != nil {
		return err
	}

	var missing []state.HostedRow
	for _, r := range rows {
		_, err := store.Stat(ctx, "blobs/sha256/"+r.SHA256)
		switch {
		case err == nil:
			// present
		case errors.Is(err, api.ErrNotFound):
			missing = append(missing, r)
		default:
			// A storage error is not an absence: reporting it as "missing"
			// would send us fetching blobs we already have, and hide a
			// broken backend behind a busy-looking backfill.
			return fmt.Errorf("check blob %s: %w", truncate(r.SHA256, 12), err)
		}
	}
	if len(missing) == 0 {
		_, _ = fmt.Fprintln(out, "every hosted coordinate has its blob locally")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "FEED\tPATH\tSHA256\tSIZE")
	for _, r := range missing {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", r.Feed, r.Path, truncate(r.SHA256, 12), r.Size)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if dryRun {
		_, _ = fmt.Fprintf(out,
			"\n%d blob(s) missing. Re-run with -dry-run=false to fetch them from peers.\n", len(missing))
		return nil
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	httpClient := &http.Client{Timeout: 10 * time.Minute}
	clients, err := peerClients(cfg.Replication, cfg.Site.Name, httpClient, logger)
	if err != nil {
		return err
	}
	if peer != "" {
		filtered := clients[:0]
		for _, c := range clients {
			if c.Name() == peer {
				filtered = append(filtered, c)
			}
		}
		clients = filtered
		if len(clients) == 0 {
			return fmt.Errorf("no peer named %q is configured", peer)
		}
	}

	var fetched, failed int
	for _, r := range missing {
		var ok bool
		for _, c := range clients {
			if err := c.FetchBlob(ctx, store, r.SHA256, r.Size); err != nil {
				logger.Debug("backfill attempt failed",
					"peer", c.Name(), "sha256", r.SHA256[:12], "error", err)
				continue
			}
			ok = true
			break
		}
		if ok {
			fetched++
		} else {
			failed++
			_, _ = fmt.Fprintf(out, "no peer has %s (%s %s)\n", truncate(r.SHA256, 12), r.Feed, r.Path)
		}
	}
	_, _ = fmt.Fprintf(out, "\nbackfill done: %d fetched, %d still missing\n", fetched, failed)
	if failed > 0 {
		return fmt.Errorf("%d blob(s) could not be fetched from any peer", failed)
	}
	return nil
}

// replTrustReset drops a peer's pinned identity. It exists so recovering a
// site that lost its database is an explicit operator decision rather than
// hand-edited SQL — and so that it stays a decision, not an automatic
// re-pin (invariant 14: nothing but a human widens trust).
func replTrustReset(ctx context.Context, db *state.DB, cfg *config.Config, peer string, out io.Writer) error {
	if peer == "" {
		return errors.New("trust-reset needs -peer <name>")
	}
	// Take the poll lease first: a cycle already running would otherwise
	// write the pre-reset cursor straight back over the reset.
	var old string
	var reset, dropped int64
	ran, err := db.TryLease(ctx, "repl-poll:"+cfg.Site.Name+":"+peer, func(ctx context.Context) error {
		// Idempotent and atomic: an operator who re-runs it after a partial
		// failure must not be left with a dropped pin and stale cursors.
		var err error
		old, reset, dropped, err = db.ResetPeerTrust(ctx, peer)
		return err
	})
	if err != nil {
		return err
	}
	if !ran {
		return fmt.Errorf("a poll cycle for peer %q is running; try again in a moment", peer)
	}
	if old == "" {
		_, _ = fmt.Fprintf(out, "peer %s had no pinned identity; cursors reset anyway (%d) and %d stale journal entr(ies) dropped\n",
			peer, reset, dropped)
		return nil
	}
	_, _ = fmt.Fprintf(out,
		"forgot the pinned identity %s for peer %s, reset %d cursor(s) and dropped %d stale journal entr(ies); "+
			"the next handshake re-pins it\n", old, peer, reset, dropped)
	return nil
}

// replQuarantine blocks a coordinate everywhere. It is the operator half of
// invariant 14: replication carries decisions that REMOVE access, and this
// is how one is made.
func replQuarantine(ctx context.Context, db *state.DB, cfg *config.Config,
	feed, coordinate, reason, detail string, out io.Writer) error {
	if feed == "" || coordinate == "" {
		return errors.New("quarantine needs -feed and -coordinate")
	}
	if reason == "" {
		reason = "manual"
	}
	if reason == "cross_site_conflict" {
		return errors.New("cross_site_conflict is derived from recorded conflicts; use `repl resolve` instead")
	}
	if err := writeQuarantineDecision(ctx, db, cfg, feed, coordinate, reason, detail, true); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "quarantined %s %s (reason %s); the decision replicates to every site\n",
		feed, coordinate, reason)
	return nil
}

// replRelease lifts one quarantine reason.
func replRelease(ctx context.Context, db *state.DB, cfg *config.Config,
	feed, coordinate, reason string, out io.Writer) error {
	if feed == "" || coordinate == "" {
		return errors.New("release needs -feed and -coordinate")
	}
	if reason == "" {
		reason = "manual"
	}
	if reason == "cross_site_conflict" {
		return errors.New("a conflict quarantine lifts itself when the conflict is resolved; use `repl resolve`")
	}
	if err := writeQuarantineDecision(ctx, db, cfg, feed, coordinate, reason, "", false); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "released %s %s (reason %s); the decision replicates to every site\n",
		feed, coordinate, reason)
	return nil
}

// writeQuarantineDecision applies the decision locally and journals it in
// one transaction, so peers cannot see a half-applied takedown.
func writeQuarantineDecision(ctx context.Context, db *state.DB, cfg *config.Config,
	feed, coordinate, reason, detail string, active bool) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Journal FIRST, then apply with the journal entry's own stamp: peers
	// order the decision by that stamp, and stamping the local write
	// separately would order it differently here than everywhere else.
	var stamp state.HLC
	if cfg.Replication.Enabled {
		writer := repl.NewWriter(cfg.Site.Name)
		var entry state.JournalEntry
		if active {
			entry, err = writer.AppendQuarantineEntry(ctx, tx, feed, coordinate, reason, detail)
		} else {
			entry, err = writer.AppendQuarantineReleaseEntry(ctx, tx, feed, coordinate, reason)
		}
		if err != nil {
			return err
		}
		stamp = entry.HLC
	} else {
		var wall, logical int64
		if err := tx.QueryRow(ctx,
			"SELECT hlc_wall, hlc_logical FROM repl_hlc_now()").Scan(&wall, &logical); err != nil {
			return fmt.Errorf("stamp decision: %w", err)
		}
		stamp = state.HLC{Wall: wall, Logical: logical}
	}
	if err := repl.ApplyQuarantineDecisionTx(ctx, tx, feed, coordinate, reason, detail,
		active, stamp); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
