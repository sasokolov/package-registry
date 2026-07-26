package repl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sasokolov/package-registry/core/state"
)

// pgxTx is the transaction handle the applier works with. It is pgx.Tx
// itself: the applier never needs a narrower surface, and aliasing keeps
// the signatures readable.
type pgxTx = pgx.Tx

// hostedState reads the current digest and mutability of a coordinate.
func hostedState(ctx context.Context, tx pgxTx, feed, path string) (sha256 string, mutable, found bool, err error) {
	err = tx.QueryRow(ctx,
		"SELECT sha256, mutable FROM hosted_manifests WHERE feed=$1 AND path=$2", feed, path).
		Scan(&sha256, &mutable)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, fmt.Errorf("read hosted manifest: %w", err)
	}
	return sha256, mutable, true, nil
}

// isNewerThanStored compares an incoming event's HLC with the stored row's
// last update, so mutable pointers converge deterministically.
func isNewerThanStored(ctx context.Context, tx pgxTx, feed, path string,
	hlc state.HLC, incomingSHA, storedSHA string) (bool, error) {
	var storedWall int64
	var storedLogical int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE((metadata->>'hlc_wall')::bigint, 0),
		       COALESCE((metadata->>'hlc_logical')::bigint, 0)
		  FROM hosted_manifests WHERE feed=$1 AND path=$2`, feed, path).
		Scan(&storedWall, &storedLogical)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read stored hlc: %w", err)
	}
	stored := state.HLC{Wall: storedWall, Logical: storedLogical}
	if stored.Before(hlc) {
		return true, nil
	}
	// Equal timestamps: break the tie by digest so every site picks the
	// same value.
	if stored == hlc && incomingSHA < storedSHA {
		return true, nil
	}
	return false, nil
}

// insertHosted stores a replicated coordinate, stamping the event's HLC into
// the metadata so later mutable updates can be ordered.
func insertHosted(ctx context.Context, tx pgxTx, p ManifestPut, e state.JournalEntry) error {
	checksums, metadata, err := encodeMaps(p, e)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO hosted_manifests
			(feed, path, coordinate, sha256, size, checksums, metadata, mutable,
			 origin, site, published_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'replication',$9,$10)
		ON CONFLICT ON CONSTRAINT hosted_manifests_feed_path_key DO NOTHING`,
		p.Feed, p.Path, p.Coord, p.SHA256, p.Size, checksums, metadata, p.Mutable,
		e.OriginSite, p.Publisher)
	if err != nil {
		return fmt.Errorf("insert replicated manifest: %w", err)
	}
	return nil
}

// updateHosted replaces the stored digest of a coordinate.
func updateHosted(ctx context.Context, tx pgxTx, p ManifestPut, e state.JournalEntry) error {
	checksums, metadata, err := encodeMaps(p, e)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE hosted_manifests
		   SET sha256=$3, size=$4, checksums=$5, metadata=$6, mutable=$7,
		       origin='replication', site=$8, published_by=$9, updated_at=now()
		 WHERE feed=$1 AND path=$2`,
		p.Feed, p.Path, p.SHA256, p.Size, checksums, metadata, p.Mutable,
		e.OriginSite, p.Publisher)
	if err != nil {
		return fmt.Errorf("update replicated manifest: %w", err)
	}
	return nil
}

func encodeMaps(p ManifestPut, e state.JournalEntry) (checksums, metadata []byte, err error) {
	cs := p.Checksums
	if cs == nil {
		cs = map[string]string{}
	}
	meta := map[string]string{}
	for k, v := range p.Metadata {
		meta[k] = v
	}
	// The originating HLC travels with the row so mutable coordinates can be
	// ordered without consulting the journal again.
	meta["hlc_wall"] = fmt.Sprintf("%d", e.HLC.Wall)
	meta["hlc_logical"] = fmt.Sprintf("%d", e.HLC.Logical)

	checksums, err = json.Marshal(cs)
	if err != nil {
		return nil, nil, fmt.Errorf("encode checksums: %w", err)
	}
	metadata, err = json.Marshal(meta)
	if err != nil {
		return nil, nil, fmt.Errorf("encode metadata: %w", err)
	}
	return checksums, metadata, nil
}

// quarantineTx blocks a coordinate inside the applier's transaction.
func quarantineTx(ctx context.Context, tx pgxTx, feed, coordinate, reason, detail string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO quarantine (feed, coordinate, reason, detail)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT ON CONSTRAINT quarantine_feed_coord_reason_key
		DO UPDATE SET detail = EXCLUDED.detail, released_at = NULL`,
		feed, coordinate, reason, detail)
	if err != nil {
		return fmt.Errorf("quarantine coordinate: %w", err)
	}
	return nil
}

// recordConflictTx stores both sides of a K1 conflict.
func recordConflictTx(ctx context.Context, tx pgxTx, p ManifestPut,
	winner, loser, winnerSite, loserSite string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO publish_conflicts
			(feed, path, coordinate, winner_sha256, loser_sha256, winner_site, loser_site)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		p.Feed, p.Path, p.Coord, winner, loser, winnerSite, loserSite)
	if err != nil {
		return fmt.Errorf("record conflict: %w", err)
	}
	return nil
}
