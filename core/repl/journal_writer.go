package repl

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sasokolov/package-registry/core/api"
	"github.com/sasokolov/package-registry/core/state"
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
	_, err := state.AppendJournal(ctx, tx, w.site, KindQuarantineSet, QuarantineSet{
		Feed: feed, Coordinate: coordinate, Reason: reason, Detail: detail,
	})
	return err
}

// AppendQuarantineRelease records lifting a quarantine.
func (w *Writer) AppendQuarantineRelease(ctx context.Context, tx pgx.Tx, feed, coordinate, reason string) error {
	_, err := state.AppendJournal(ctx, tx, w.site, KindQuarantineRelease, QuarantineRelease{
		Feed: feed, Coordinate: coordinate, Reason: reason,
	})
	return err
}

// AppendConflictResolve records an operator's resolution of a K1 conflict.
func (w *Writer) AppendConflictResolve(ctx context.Context, tx pgx.Tx, feed, path, coord, keepSHA, operator string) error {
	_, err := state.AppendJournal(ctx, tx, w.site, KindConflictResolve, ConflictResolve{
		Feed: feed, Path: path, Coord: coord, KeepSHA: keepSHA, Operator: operator,
	})
	return err
}
