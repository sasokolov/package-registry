package repl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sasokolov/package-registry/core/state"
)

// BlobFetcher retrieves a blob by digest from a peer and stores it locally.
// The transfer is self-verifying: the key IS the checksum (invariant 5).
type BlobFetcher interface {
	EnsureBlob(ctx context.Context, sha256 string, size int64, fromPeer string) error
	HasBlob(ctx context.Context, sha256 string) bool
}

// Reindexer rebuilds a feed's local indexes after its manifest set changed.
type Reindexer interface {
	ReindexFeed(ctx context.Context, feedName string) error
}

// Projector mirrors a replicated coordinate into the blob store, which is
// what the read path actually consults (and what keeps hosted content
// servable while PostgreSQL is down, invariant 7).
type Projector interface {
	WriteReplicatedManifest(ctx context.Context, feed, path, sha256 string, size int64,
		checksums, metadata map[string]string, originSite, publisher string) error
}

// Applier merges journal entries into local state.
type Applier struct {
	db      *state.DB
	site    string
	blobs   BlobFetcher
	reindex Reindexer
	project Projector
	logger  *slog.Logger
	audit   *slog.Logger
	metrics *Metrics
	maxSkew time.Duration
	now     func() time.Time
	eager   func(feed string) bool
}

// ApplierOptions wires an Applier.
type ApplierOptions struct {
	DB      *state.DB
	Site    string
	Blobs   BlobFetcher
	Reindex Reindexer
	Project Projector
	Logger  *slog.Logger
	Audit   *slog.Logger
	Metrics *Metrics
	MaxSkew time.Duration
	Now     func() time.Time
	// Eager reports whether a feed replicates blobs ahead of demand.
	Eager func(feed string) bool
}

// NewApplier builds the applier.
func NewApplier(o ApplierOptions) *Applier {
	a := &Applier{
		db: o.DB, site: o.Site, blobs: o.Blobs, reindex: o.Reindex, project: o.Project,
		logger: o.Logger, audit: o.Audit, metrics: o.Metrics,
		maxSkew: o.MaxSkew, now: o.Now, eager: o.Eager,
	}
	if a.logger == nil {
		a.logger = slog.Default()
	}
	if a.audit == nil {
		a.audit = a.logger
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.maxSkew <= 0 {
		a.maxSkew = 5 * time.Minute
	}
	if a.eager == nil {
		a.eager = func(string) bool { return false }
	}
	return a
}

// SetBlobs attaches the blob fetcher (the manager, which needs the applier
// to exist first).
func (a *Applier) SetBlobs(b BlobFetcher) { a.blobs = b }

// ApplyBatch applies entries from one peer stream and advances the cursor in
// the same transaction, so a crash cannot skip events. Entries that cannot
// be applied yet are parked (never blocking the stream) and retried later.
func (a *Applier) ApplyBatch(ctx context.Context, peer string, entries []state.JournalEntry) (touchedFeeds map[string]bool, err error) {
	touchedFeeds = map[string]bool{}
	if len(entries) == 0 {
		return touchedFeeds, nil
	}

	for _, e := range entries {
		if e.OriginSite == a.site {
			// Our own event coming back through the mesh: nothing to do.
			continue
		}
		if err := a.applyOne(ctx, peer, e, touchedFeeds); err != nil {
			return touchedFeeds, err
		}
	}
	return touchedFeeds, nil
}

// applyOne applies a single entry in its own transaction: one poisonous
// event cannot roll back a whole batch.
func (a *Applier) applyOne(ctx context.Context, peer string, e state.JournalEntry, touched map[string]bool) error {
	// Events far in the future indicate clock skew or a hostile peer: park
	// them and re-evaluate later rather than letting them win comparisons.
	if skew := time.Duration(e.HLC.Wall-a.now().UnixMilli()) * time.Millisecond; skew > a.maxSkew {
		a.logger.Warn("parking event from the future",
			"origin", e.OriginSite, "seq", e.OriginSeq, "kind", e.Kind, "skew", skew)
		a.metrics.parked()
		return a.db.ParkEvent(ctx, e, fmt.Sprintf("clock skew %s", skew.Truncate(time.Second)))
	}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	fresh, err := a.db.ApplyForeignJournal(ctx, tx, e)
	if err != nil {
		return err
	}
	if !fresh {
		// Already applied: idempotent replay.
		return tx.Commit(ctx)
	}

	if err := a.dispatch(ctx, tx, peer, e, touched); err != nil {
		if errors.Is(err, errPark) {
			_ = tx.Rollback(ctx)
			a.metrics.parked()
			return a.db.ParkEvent(ctx, e, err.Error())
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit applied event: %w", err)
	}
	a.metrics.applied(e.Kind)
	return nil
}

// errPark marks an event that should be parked rather than retried inline.
var errPark = errors.New("event parked")

func parkf(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), errPark)
}

func (a *Applier) dispatch(ctx context.Context, tx pgxTx, peer string, e state.JournalEntry, touched map[string]bool) error {
	switch e.Kind {
	case KindManifestPut:
		return a.applyManifestPut(ctx, tx, peer, e, touched)
	case KindBlobAvailable:
		return a.applyBlobAvailable(ctx, peer, e)
	case KindTokenRevoke:
		return a.applyTokenRevoke(ctx, tx, e)
	case KindQuarantineSet:
		return a.applyQuarantineSet(ctx, tx, e)
	case KindQuarantineRelease:
		return a.applyQuarantineRelease(ctx, tx, e)
	case KindConflictResolve:
		return a.applyConflictResolve(ctx, tx, e, touched)
	default:
		// Unknown kind: a newer peer speaks a dialect we do not. Park it,
		// alert, and let an upgraded binary retry — never silently drop.
		a.logger.Error("unknown replication event kind, parking",
			"kind", e.Kind, "origin", e.OriginSite, "seq", e.OriginSeq,
			"schema_version", e.SchemaVersion)
		return parkf("unknown event kind %q (schema version %d)", e.Kind, e.SchemaVersion)
	}
}

// applyManifestPut merges a published coordinate, applying rule K1 on
// conflict.
func (a *Applier) applyManifestPut(ctx context.Context, tx pgxTx, peer string,
	e state.JournalEntry, touched map[string]bool) error {

	var p ManifestPut
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return parkf("malformed manifest_put payload: %v", err)
	}
	if p.Feed == "" || p.Path == "" || p.SHA256 == "" {
		return parkf("incomplete manifest_put payload")
	}

	// Eager feeds must hold the bytes before the coordinate becomes
	// visible, so a reader never sees a manifest without its blob.
	if a.eager(p.Feed) && !a.blobs.HasBlob(ctx, p.SHA256) {
		if err := a.blobs.EnsureBlob(ctx, p.SHA256, p.Size, peer); err != nil {
			return parkf("blob %s unavailable: %v", p.SHA256[:12], err)
		}
	}

	existingSHA, existingMutable, found, err := hostedState(ctx, tx, p.Feed, p.Path)
	if err != nil {
		return err
	}

	switch {
	case !found:
		if err := insertHosted(ctx, tx, p, e); err != nil {
			return err
		}
	case p.Mutable && existingMutable:
		// Mutable pointers (dist-tags, SNAPSHOT aliases) converge by HLC:
		// the newest write wins and both sides agree on which that is. The
		// watermark advances even when the digest is unchanged — otherwise
		// a third value could win or lose depending on arrival order.
		newer, err := isNewerThanStored(ctx, tx, p.Feed, p.Path, e.HLC, p.SHA256, existingSHA)
		if err != nil {
			return err
		}
		if !newer {
			return nil
		}
		if err := updateHosted(ctx, tx, p, e); err != nil {
			return err
		}
	case existingSHA == p.SHA256:
		// Immutable coordinate, same bytes: idempotent.
		return nil
	default:
		// Rule K1: two different byte sequences for one immutable
		// coordinate. Canonical state is the lexicographically smallest
		// sha256 — content-derived, so every site agrees without
		// coordination and no clock can be gamed. The coordinate is
		// quarantined until an operator resolves it.
		winner, loser := p.SHA256, existingSHA
		winnerSite, loserSite := e.OriginSite, a.site
		if existingSHA < p.SHA256 {
			winner, loser = existingSHA, p.SHA256
			winnerSite, loserSite = a.site, e.OriginSite
		}
		if winner != existingSHA {
			if err := updateHosted(ctx, tx, p, e); err != nil {
				return err
			}
		}
		if err := quarantineTx(ctx, tx, p.Feed, p.Coord, "cross_site_conflict",
			fmt.Sprintf("%s published different content for %s (%s vs %s)",
				e.OriginSite, p.Path, short(winner), short(loser))); err != nil {
			return err
		}
		if err := recordConflictTx(ctx, tx, p, winner, loser, winnerSite, loserSite); err != nil {
			return err
		}
		a.metrics.conflict()
		a.audit.Error("cross-site publish conflict, coordinate quarantined",
			"feed", p.Feed, "path", p.Path, "coordinate", p.Coord,
			"winner_sha256", winner, "loser_sha256", loser,
			"winner_site", winnerSite, "loser_site", loserSite)
	}

	// The read path serves from the blob store, so a replicated coordinate
	// is only visible once its projection exists.
	if a.project != nil {
		if err := a.project.WriteReplicatedManifest(ctx, p.Feed, p.Path, currentSHA(p, existingSHA, found),
			p.Size, p.Checksums, p.Metadata, e.OriginSite, p.Publisher); err != nil {
			a.logger.Error("writing the replicated manifest projection failed",
				"feed", p.Feed, "path", p.Path, "error", err)
		}
	}

	touched[p.Feed] = true
	return nil
}

// currentSHA is the digest the coordinate now resolves to: the incoming one
// unless rule K1 kept the smaller existing digest.
func currentSHA(p ManifestPut, existingSHA string, found bool) string {
	if found && existingSHA != "" && existingSHA < p.SHA256 {
		return existingSHA
	}
	return p.SHA256
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// applyBlobAvailable fetches the blob when the local policy wants it eagerly.
func (a *Applier) applyBlobAvailable(ctx context.Context, peer string, e state.JournalEntry) error {
	var p BlobAvailable
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return parkf("malformed blob_available payload: %v", err)
	}
	if p.SHA256 == "" || a.blobs.HasBlob(ctx, p.SHA256) {
		return nil
	}
	// Best effort: a missing blob is fetched on demand by the read path.
	if err := a.blobs.EnsureBlob(ctx, p.SHA256, p.Size, peer); err != nil {
		a.logger.Debug("eager blob fetch failed, will fetch on demand",
			"sha256", short(p.SHA256), "peer", peer, "error", err)
	}
	return nil
}

// applyTokenRevoke revokes a token by hash. Revocation is sticky: it can
// only ever remove access (invariant 14).
func (a *Applier) applyTokenRevoke(ctx context.Context, tx pgxTx, e state.JournalEntry) error {
	var p TokenRevoke
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return parkf("malformed token_revoke payload: %v", err)
	}
	if len(p.Hash) != 64 {
		return parkf("token_revoke carries an implausible hash")
	}
	if _, err := tx.Exec(ctx,
		"UPDATE tokens SET revoked_at = COALESCE(revoked_at, now()), updated_at = now() WHERE hash = $1",
		p.Hash); err != nil {
		return fmt.Errorf("apply token revoke: %w", err)
	}
	a.audit.Warn("token revoked by replication", "hash_prefix", p.Hash[:8], "origin", e.OriginSite)
	return nil
}

func (a *Applier) applyQuarantineSet(ctx context.Context, tx pgxTx, e state.JournalEntry) error {
	var p QuarantineSet
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return parkf("malformed quarantine_set payload: %v", err)
	}
	return quarantineTx(ctx, tx, p.Feed, p.Coordinate, p.Reason, p.Detail)
}

func (a *Applier) applyQuarantineRelease(ctx context.Context, tx pgxTx, e state.JournalEntry) error {
	var p QuarantineRelease
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return parkf("malformed quarantine_release payload: %v", err)
	}
	// Release only the named reason: lifting a manual takedown must not
	// also clear a cross-site conflict (and vice versa).
	reason := p.Reason
	if reason == "" {
		reason = "manual"
	}
	_, err := tx.Exec(ctx, `
		UPDATE quarantine SET released_at = now()
		 WHERE feed=$1 AND coordinate=$2 AND reason=$3 AND released_at IS NULL`,
		p.Feed, p.Coordinate, reason)
	return err
}

// applyConflictResolve applies an operator's choice: the chosen digest
// becomes canonical everywhere and the quarantine is lifted.
func (a *Applier) applyConflictResolve(ctx context.Context, tx pgxTx, e state.JournalEntry, touched map[string]bool) error {
	var p ConflictResolve
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return parkf("malformed conflict_resolve payload: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE hosted_manifests SET sha256 = $3, updated_at = now()
		 WHERE feed = $1 AND path = $2`, p.Feed, p.Path, p.KeepSHA); err != nil {
		return fmt.Errorf("apply conflict resolution: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE quarantine SET released_at = now()
		 WHERE feed=$1 AND coordinate=$2 AND reason='cross_site_conflict' AND released_at IS NULL`,
		p.Feed, p.Coord); err != nil {
		return fmt.Errorf("release quarantine after resolution: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE publish_conflicts SET resolved_at = now(), resolved_sha256 = $3
		 WHERE feed=$1 AND path=$2 AND resolved_at IS NULL`, p.Feed, p.Path, p.KeepSHA); err != nil {
		return fmt.Errorf("close conflict record: %w", err)
	}
	a.audit.Warn("cross-site conflict resolved by operator",
		"feed", p.Feed, "path", p.Path, "keep_sha256", p.KeepSHA,
		"operator", p.Operator, "origin", e.OriginSite)
	touched[p.Feed] = true
	return nil
}

// RetryParked re-attempts parked events (clock skew that has passed, blobs
// that arrived, a binary that now understands the event kind).
func (a *Applier) RetryParked(ctx context.Context, peer string) error {
	entries, reasons, err := a.db.ParkedEvents(ctx, 100)
	if err != nil {
		return err
	}
	touched := map[string]bool{}
	for i, e := range entries {
		tx, err := a.db.Begin(ctx)
		if err != nil {
			return err
		}
		err = a.dispatch(ctx, tx, peer, e, touched)
		if err != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(err, errPark) {
				continue // still not applicable
			}
			return fmt.Errorf("retry parked %s/%d (parked for %q): %w",
				e.OriginSite, e.OriginSeq, reasons[i], err)
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		if err := a.db.UnparkEvent(ctx, e.OriginSite, e.OriginSeq); err != nil {
			return err
		}
		a.logger.Info("parked event applied on retry",
			"origin", e.OriginSite, "seq", e.OriginSeq, "kind", e.Kind)
	}
	return a.reindexTouched(ctx, touched)
}

// reindexTouched rebuilds indexes of feeds whose manifest set changed.
func (a *Applier) reindexTouched(ctx context.Context, touched map[string]bool) error {
	if a.reindex == nil {
		return nil
	}
	for feed := range touched {
		if err := a.reindex.ReindexFeed(ctx, feed); err != nil {
			a.logger.Error("reindex after replication failed", "feed", feed, "error", err)
		}
	}
	return nil
}

// ReindexTouched is the exported entry point used by the puller.
func (a *Applier) ReindexTouched(ctx context.Context, touched map[string]bool) error {
	return a.reindexTouched(ctx, touched)
}
