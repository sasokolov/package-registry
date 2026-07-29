package repl

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/core/state"
)

// Writer records local actions in the replication journal. It is the only
// producer of events: everything peers learn about this site passes here.
type Writer struct {
	site string
}

// NewWriter builds the journal writer for a site.
func NewWriter(site string) *Writer { return &Writer{site: site} }

// AppendManifestPut records a publish. It runs inside the publisher's
// transaction, so the coordinate and its announcement commit together
// (transactional outbox).
func (w *Writer) AppendManifestPut(ctx context.Context, tx pgx.Tx, req api.PublishRequest) error {
	_, err := state.AppendJournal(ctx, tx, w.site, KindManifestPut, ManifestPut{
		Feed:      req.Feed.Name,
		Path:      req.Path,
		Coord:     req.Coord.String(),
		SHA256:    req.SHA256,
		Size:      req.Size,
		Checksums: req.Checksums,
		Metadata:  req.Metadata,
		Mutable:   req.Mutable,
		Publisher: req.Identity.String(),
	})
	if err != nil {
		return err
	}
	// Announce the bytes separately: eager peers fetch on this event, lazy
	// ones ignore it and fetch on demand.
	_, err = state.AppendJournal(ctx, tx, w.site, KindBlobAvailable, BlobAvailable{
		SHA256: req.SHA256,
		Size:   req.Size,
	})
	return err
}

// AppendTokenRevoke records a token revocation (hash only, invariants 12
// and 14).
func (w *Writer) AppendTokenRevoke(ctx context.Context, tx pgx.Tx, hash string) error {
	_, err := state.AppendJournal(ctx, tx, w.site, KindTokenRevoke, TokenRevoke{Hash: hash})
	return err
}

// AppendQuarantine records a quarantine decision.
func (w *Writer) AppendQuarantine(ctx context.Context, tx pgx.Tx, feed, coordinate, reason, detail string) error {
	_, err := w.AppendQuarantineEntry(ctx, tx, feed, coordinate, reason, detail)
	return err
}

// AppendQuarantineEntry is AppendQuarantine returning the journal entry, so
// the caller can apply the decision locally with the SAME stamp peers will
// order it by.
func (w *Writer) AppendQuarantineEntry(ctx context.Context, tx pgx.Tx,
	feed, coordinate, reason, detail string) (state.JournalEntry, error) {
	return state.AppendJournal(ctx, tx, w.site, KindQuarantineSet, QuarantineSet{
		Feed: feed, Coordinate: coordinate, Reason: reason, Detail: detail,
	})
}

// AppendQuarantineRelease records lifting a quarantine.
func (w *Writer) AppendQuarantineRelease(ctx context.Context, tx pgx.Tx, feed, coordinate, reason string) error {
	_, err := w.AppendQuarantineReleaseEntry(ctx, tx, feed, coordinate, reason)
	return err
}

// AppendQuarantineReleaseEntry is AppendQuarantineRelease returning the
// journal entry and its stamp.
func (w *Writer) AppendQuarantineReleaseEntry(ctx context.Context, tx pgx.Tx,
	feed, coordinate, reason string) (state.JournalEntry, error) {
	return state.AppendJournal(ctx, tx, w.site, KindQuarantineRelease, QuarantineRelease{
		Feed: feed, Coordinate: coordinate, Reason: reason,
	})
}

// AppendConflictResolve records an operator's resolution of a K1 conflict.
func (w *Writer) AppendConflictResolve(ctx context.Context, tx pgx.Tx, feed, path, coord, keepSHA, operator string) error {
	_, err := state.AppendJournal(ctx, tx, w.site, KindConflictResolve, ConflictResolve{
		Feed: feed, Path: path, Coord: coord, KeepSHA: keepSHA, Operator: operator,
	})
	return err
}

// ApplyQuarantineDecisionTx applies an operator's quarantine decision with
// the same last-writer-wins rules the applier uses, so a local decision and
// a replicated one order identically.
func ApplyQuarantineDecisionTx(ctx context.Context, tx pgx.Tx, feed, coordinate, reason, detail string,
	active bool, hlc state.HLC) error {
	if active {
		return quarantineTx(ctx, tx, feed, coordinate, reason, detail, hlc)
	}
	return releaseQuarantineTx(ctx, tx, feed, coordinate, reason, hlc)
}

// ProjectionWriter writes the blob-store view of a coordinate. The
// pipeline's Publisher implements it; naming it here keeps the resolve
// operation in one place instead of duplicated between the applier and the
// CLI, where the two drifted apart.
type ProjectionWriter interface {
	WriteReplicatedManifest(ctx context.Context, feed, path, sha256 string, size int64,
		checksums, metadata map[string]string, originSite, publisher string) error
}

// ResolveConflict applies an operator's decision at this site and announces
// it: the coordinate is pointed at the kept digest WITH that digest's own
// size and checksums, the projection is rewritten, the conflict is closed,
// the quarantine lifted, and the decision recorded as terminal state so a
// late publish cannot re-open it. All of it commits together with the
// journal entry, so peers cannot see a half-applied resolution.
func ResolveConflict(ctx context.Context, db *state.DB, projection ProjectionWriter,
	site, feed, path, coord, keepSHA, operator string) error {
	if len(keepSHA) != 64 || !isHex(keepSHA) {
		return fmt.Errorf("keep digest %q is not a sha256 hex digest", keepSHA)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	side, ok, err := conflictSide(ctx, tx, feed, path, keepSHA)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("digest %s is not one of the recorded sides of the conflict for %s %s",
			keepSHA[:12], feed, path)
	}

	// The decision is stamped so peers order it against everything else.
	var wall, logical int64
	if err := tx.QueryRow(ctx, "SELECT hlc_wall, hlc_logical FROM repl_hlc_now()").Scan(&wall, &logical); err != nil {
		return fmt.Errorf("stamp resolution: %w", err)
	}
	decision := resolution{
		KeepSHA: side.SHA256, Size: side.Size,
		Checksums: side.Checksums, Metadata: side.Metadata,
		HLC: state.HLC{Wall: wall, Logical: logical},
	}
	if err := recordResolution(ctx, tx, feed, path, coord, decision, operator, site); err != nil {
		return err
	}
	if err := applyResolutionTx(ctx, tx, feed, path, coord, decision, site); err != nil {
		return err
	}
	if err := NewWriter(site).AppendConflictResolve(ctx, tx, feed, path, coord, decision.KeepSHA, operator); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit resolution: %w", err)
	}

	// The read path serves from the projection, so it has to follow the
	// decision too. It is a projection of committed state, hence written
	// after the commit and repaired by the repair loop if this fails.
	if projection != nil {
		if err := projection.WriteReplicatedManifest(ctx, feed, path, decision.KeepSHA,
			decision.Size, decision.Checksums, decision.Metadata, site, operator); err != nil {
			return fmt.Errorf("rewrite the resolved manifest projection: %w", err)
		}
	}
	return nil
}
