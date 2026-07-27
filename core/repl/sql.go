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

// hostedRow is the part of a stored coordinate the merge rules need.
type hostedRow struct {
	SHA256    string
	Size      int64
	Checksums map[string]string
	Metadata  map[string]string
	Mutable   bool
	// Site is where the stored bytes were published, which is what a
	// conflict record must name — not whichever site happens to be merging.
	Site string
}

// hostedState reads the stored coordinate, locking the row: the merge is a
// read-then-write decision, and two appliers (or an applier and a local
// publish) must not both act on the same pre-image.
func hostedState(ctx context.Context, tx pgxTx, feed, path string) (hostedRow, bool, error) {
	var r hostedRow
	var checksums, metadata []byte
	err := tx.QueryRow(ctx, `
		SELECT sha256, size, checksums, metadata, mutable, site
		  FROM hosted_manifests WHERE feed=$1 AND path=$2
		FOR UPDATE`, feed, path).
		Scan(&r.SHA256, &r.Size, &checksums, &metadata, &r.Mutable, &r.Site)
	if errors.Is(err, pgx.ErrNoRows) {
		return hostedRow{}, false, nil
	}
	if err != nil {
		return hostedRow{}, false, fmt.Errorf("read hosted manifest: %w", err)
	}
	r.Checksums = decodeStringMap(checksums)
	r.Metadata = decodeStringMap(metadata)
	return r, true, nil
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

// quarantineTx activates a quarantine reason as a last-writer-wins register
// stamped with the event's HLC. An older event never overwrites a newer
// decision, so set and release commute: a release that arrives before the
// set it lifts still wins, instead of matching zero rows and vanishing.
func quarantineTx(ctx context.Context, tx pgxTx, feed, coordinate, reason, detail string, hlc state.HLC) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO quarantine (feed, coordinate, reason, detail, hlc_wall, hlc_logical)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT ON CONSTRAINT quarantine_feed_coord_reason_key
		DO UPDATE SET detail = EXCLUDED.detail,
		              released_at = NULL,
		              hlc_wall = EXCLUDED.hlc_wall,
		              hlc_logical = EXCLUDED.hlc_logical
		-- Equal timestamps break towards "blocked": with no way to order
		-- two decisions, the safe one wins, and every site picks the same.
		WHERE (quarantine.hlc_wall, quarantine.hlc_logical)
		   <= (EXCLUDED.hlc_wall, EXCLUDED.hlc_logical)`,
		feed, coordinate, reason, detail, hlc.Wall, hlc.Logical)
	if err != nil {
		return fmt.Errorf("quarantine coordinate: %w", err)
	}
	return nil
}

// releaseQuarantineTx lifts one reason, creating the row if the matching
// set has not arrived yet (the release still wins by HLC when it does).
func releaseQuarantineTx(ctx context.Context, tx pgxTx, feed, coordinate, reason string, hlc state.HLC) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO quarantine (feed, coordinate, reason, detail, released_at, hlc_wall, hlc_logical)
		VALUES ($1,$2,$3,'', now(), $4,$5)
		ON CONFLICT ON CONSTRAINT quarantine_feed_coord_reason_key
		DO UPDATE SET released_at = now(),
		              hlc_wall = EXCLUDED.hlc_wall,
		              hlc_logical = EXCLUDED.hlc_logical
		-- Strict: a release with the same timestamp as a set loses, so the
		-- tie-break above ("blocked wins") holds from both directions.
		WHERE (quarantine.hlc_wall, quarantine.hlc_logical)
		    < (EXCLUDED.hlc_wall, EXCLUDED.hlc_logical)`,
		feed, coordinate, reason, hlc.Wall, hlc.Logical)
	if err != nil {
		return fmt.Errorf("release quarantine: %w", err)
	}
	return nil
}

// syncConflictQuarantineTx recomputes the cross_site_conflict quarantine of
// a coordinate from the conflicts recorded for it. It is a pure function of
// committed table state, so every site derives the same answer whatever
// order events arrived in — unlike a timestamped register, which recorded
// whichever publish happened to be applied second here.
//
// A Maven GAV spans several paths (jar, pom, sources), and each path
// conflicts on its own; the coordinate stays blocked while ANY of them is
// unresolved.
func syncConflictQuarantineTx(ctx context.Context, tx pgxTx, feed, coordinate string) error {
	var open int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM publish_conflicts
		 WHERE feed = $1 AND coordinate = $2 AND resolved_at IS NULL`,
		feed, coordinate).Scan(&open); err != nil {
		return fmt.Errorf("count open conflicts: %w", err)
	}

	if open > 0 {
		_, err := tx.Exec(ctx, `
			INSERT INTO quarantine (feed, coordinate, reason, detail, hlc_wall, hlc_logical)
			VALUES ($1,$2,'cross_site_conflict',$3,0,0)
			ON CONFLICT ON CONSTRAINT quarantine_feed_coord_reason_key
			DO UPDATE SET detail = EXCLUDED.detail, released_at = NULL`,
			feed, coordinate, fmt.Sprintf("%d unresolved cross-site conflict(s)", open))
		if err != nil {
			return fmt.Errorf("quarantine conflicted coordinate: %w", err)
		}
		return nil
	}

	// Nothing unresolved left: lift the block. Other reasons (a manual
	// takedown, a policy verdict) are separate rows and are untouched.
	if _, err := tx.Exec(ctx, `
		UPDATE quarantine SET released_at = now()
		 WHERE feed=$1 AND coordinate=$2 AND reason='cross_site_conflict' AND released_at IS NULL`,
		feed, coordinate); err != nil {
		return fmt.Errorf("release conflict quarantine: %w", err)
	}
	return nil
}

// resolution is an operator's terminal decision for a conflicted
// coordinate.
type resolution struct {
	KeepSHA   string
	Size      int64
	Checksums map[string]string
	Metadata  map[string]string
	HLC       state.HLC
}

// storedResolution reads the decision for a coordinate, if any.
func storedResolution(ctx context.Context, tx pgxTx, feed, path string) (resolution, bool, error) {
	var r resolution
	var checksums, metadata []byte
	err := tx.QueryRow(ctx, `
		SELECT keep_sha256, size, checksums, metadata, hlc_wall, hlc_logical
		  FROM conflict_resolutions WHERE feed=$1 AND path=$2`, feed, path).
		Scan(&r.KeepSHA, &r.Size, &checksums, &metadata, &r.HLC.Wall, &r.HLC.Logical)
	if errors.Is(err, pgx.ErrNoRows) {
		return resolution{}, false, nil
	}
	if err != nil {
		return resolution{}, false, fmt.Errorf("read conflict resolution: %w", err)
	}
	r.Checksums = decodeStringMap(checksums)
	r.Metadata = decodeStringMap(metadata)
	return r, true, nil
}

// recordResolution stores the decision so later merges honour it.
func recordResolution(ctx context.Context, tx pgxTx, feed, path, coord string,
	r resolution, operator, decidedBy string) error {
	checksums, err := json.Marshal(orEmpty(r.Checksums))
	if err != nil {
		return fmt.Errorf("encode resolution checksums: %w", err)
	}
	metadata, err := json.Marshal(orEmpty(r.Metadata))
	if err != nil {
		return fmt.Errorf("encode resolution metadata: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO conflict_resolutions
			(feed, path, coordinate, keep_sha256, size, checksums, metadata,
			 operator, decided_by, hlc_wall, hlc_logical)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (feed, path) DO UPDATE
		   SET keep_sha256 = EXCLUDED.keep_sha256,
		       size = EXCLUDED.size,
		       checksums = EXCLUDED.checksums,
		       metadata = EXCLUDED.metadata,
		       operator = EXCLUDED.operator,
		       decided_by = EXCLUDED.decided_by,
		       hlc_wall = EXCLUDED.hlc_wall,
		       hlc_logical = EXCLUDED.hlc_logical,
		       decided_at = now()
		-- Two operators can decide one coordinate at the same instant; the
		-- digest breaks the tie so every site keeps the same bytes.
		WHERE (conflict_resolutions.hlc_wall, conflict_resolutions.hlc_logical, conflict_resolutions.keep_sha256)
		    < (EXCLUDED.hlc_wall, EXCLUDED.hlc_logical, EXCLUDED.keep_sha256)`,
		feed, path, coord, r.KeepSHA, r.Size, checksums, metadata,
		operator, decidedBy, r.HLC.Wall, r.HLC.Logical)
	if err != nil {
		return fmt.Errorf("record conflict resolution: %w", err)
	}
	return nil
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func decodeStringMap(raw []byte) map[string]string {
	out := map[string]string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// conflictSide looks up the recorded manifest data of one side of an open
// conflict. It is how a resolution is validated: only a digest this site
// actually saw published can be chosen.
func conflictSide(ctx context.Context, tx pgxTx, feed, path, sha256hex string) (sideMeta, bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT winner_sha256, loser_sha256, winner_meta, loser_meta
		  FROM publish_conflicts WHERE feed=$1 AND path=$2`, feed, path)
	if err != nil {
		return sideMeta{}, false, fmt.Errorf("read conflict sides: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var winnerSHA, loserSHA string
		var winnerRaw, loserRaw []byte
		if err := rows.Scan(&winnerSHA, &loserSHA, &winnerRaw, &loserRaw); err != nil {
			return sideMeta{}, false, err
		}
		for _, candidate := range []struct {
			sha string
			raw []byte
		}{{winnerSHA, winnerRaw}, {loserSHA, loserRaw}} {
			if candidate.sha != sha256hex {
				continue
			}
			var meta sideMeta
			if len(candidate.raw) > 0 {
				_ = json.Unmarshal(candidate.raw, &meta)
			}
			meta.SHA256 = candidate.sha
			if meta.Size == 0 && len(meta.Checksums) == 0 {
				// A conflict recorded before this data was carried (or by
				// an older binary). Recover the side's size and checksums:
				// from the stored row when it is the one being kept, and
				// otherwise from an already-recorded resolution or from
				// the blob itself, so a resolution never advertises a size
				// of zero for real bytes.
				if stored, found, err := hostedState(ctx, tx, feed, path); err == nil && found &&
					stored.SHA256 == candidate.sha {
					meta.Size = stored.Size
					meta.Checksums = stored.Checksums
					meta.Metadata = stored.Metadata
				}
			}
			if meta.Size == 0 {
				// Last resort: the blob is content-addressed, so its length
				// is authoritative and cheap to read.
				if size, err := blobSize(ctx, tx, candidate.sha); err == nil && size > 0 {
					meta.Size = size
				}
			}
			return meta, true, nil
		}
	}
	return sideMeta{}, false, rows.Err()
}

// conflictSideSite reports which site published a conflicting digest.
func conflictSideSite(ctx context.Context, tx pgxTx, feed, path, sha256hex string) (string, error) {
	var site string
	err := tx.QueryRow(ctx, `
		SELECT CASE WHEN winner_sha256 = $3 THEN winner_site ELSE loser_site END
		  FROM publish_conflicts
		 WHERE feed=$1 AND path=$2 AND (winner_sha256 = $3 OR loser_sha256 = $3)
		 LIMIT 1`, feed, path, sha256hex).Scan(&site)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return site, err
}

// blobSize recovers a blob's length from any coordinate that already
// records it, which is enough to keep a resolution from advertising zero.
func blobSize(ctx context.Context, tx pgxTx, sha256hex string) (int64, error) {
	var size int64
	err := tx.QueryRow(ctx,
		"SELECT size FROM hosted_manifests WHERE sha256 = $1 AND size > 0 LIMIT 1",
		sha256hex).Scan(&size)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return size, err
}

// applyResolutionTx points the coordinate at the kept digest with that
// digest's own size and checksums, closes the conflict and lifts the
// conflict quarantine.
func applyResolutionTx(ctx context.Context, tx pgxTx, feed, path, coord string,
	r resolution, decidedBy string) error {
	// Where the kept bytes came from: the conflict record knows, and it is
	// the same answer on every site.
	keptSite := decidedBy
	if site, err := conflictSideSite(ctx, tx, feed, path, r.KeepSHA); err == nil && site != "" {
		keptSite = site
	}
	checksums, err := json.Marshal(orEmpty(r.Checksums))
	if err != nil {
		return fmt.Errorf("encode resolved checksums: %w", err)
	}
	metadata, err := json.Marshal(orEmpty(r.Metadata))
	if err != nil {
		return fmt.Errorf("encode resolved metadata: %w", err)
	}
	// Upsert, not update: a site bootstrapping from a snapshot learns the
	// resolution BEFORE the coordinate itself, and an update against a row
	// that does not exist yet would silently drop the package.
	// Upsert, not update: a site bootstrapping from a snapshot learns the
	// resolution BEFORE the coordinate itself, and an update against a row
	// that does not exist yet would silently drop the package. The site
	// column keeps naming where the KEPT bytes were published — attributing
	// them to whoever resolved the conflict would make provenance depend on
	// who happened to run the command.
	if _, err := tx.Exec(ctx, `
		INSERT INTO hosted_manifests
			(feed, path, coordinate, sha256, size, checksums, metadata, mutable,
			 origin, site, published_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,false,'replication',$8,'conflict-resolution')
		ON CONFLICT ON CONSTRAINT hosted_manifests_feed_path_key
		DO UPDATE SET sha256=EXCLUDED.sha256, size=EXCLUDED.size,
		              checksums=EXCLUDED.checksums, metadata=EXCLUDED.metadata,
		              updated_at=now()`,
		feed, path, coord, r.KeepSHA, r.Size, checksums, metadata, keptSite); err != nil {
		return fmt.Errorf("apply conflict resolution: %w", err)
	}
	// Close this PATH's conflicts, then recompute the coordinate's block:
	// a sibling path of the same coordinate may still be unresolved, and
	// releasing on its behalf would serve bytes nobody chose.
	if _, err := tx.Exec(ctx, `
		UPDATE publish_conflicts SET resolved_at = now(), resolved_sha256 = $3
		 WHERE feed=$1 AND path=$2 AND resolved_at IS NULL`, feed, path, r.KeepSHA); err != nil {
		return fmt.Errorf("close conflict record: %w", err)
	}
	return syncConflictQuarantineTx(ctx, tx, feed, coord)
}

// recordResolvedConflictTx records a publish that arrived after an operator
// had already decided the coordinate. It is filed as an already-resolved
// conflict: informative for audit, and never blocking.
func recordResolvedConflictTx(ctx context.Context, tx pgxTx, p ManifestPut,
	kept, rejected sideMeta, rejectedSite string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM publish_conflicts
			 WHERE feed=$1 AND path=$2 AND (winner_sha256=$3 OR loser_sha256=$3))`,
		p.Feed, p.Path, rejected.SHA256).Scan(&exists); err != nil {
		return fmt.Errorf("check recorded conflict: %w", err)
	}
	if exists {
		return nil
	}
	keptJSON, err := json.Marshal(kept)
	if err != nil {
		return fmt.Errorf("encode kept side: %w", err)
	}
	rejectedJSON, err := json.Marshal(rejected)
	if err != nil {
		return fmt.Errorf("encode rejected side: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO publish_conflicts
			(feed, path, coordinate, winner_sha256, loser_sha256, winner_site, loser_site,
			 winner_meta, loser_meta, resolved_at, resolved_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, now(), $4)`,
		p.Feed, p.Path, p.Coord, kept.SHA256, rejected.SHA256, "resolved", rejectedSite,
		keptJSON, rejectedJSON)
	if err != nil {
		return fmt.Errorf("record post-resolution conflict: %w", err)
	}
	return nil
}

// importConflictTx records a conflict learned from a peer's snapshot and
// re-derives the coordinate's block from it.
func importConflictTx(ctx context.Context, tx pgxTx, c ConflictRecord) error {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM publish_conflicts
			 WHERE feed=$1 AND path=$2 AND winner_sha256=$3 AND loser_sha256=$4)`,
		c.Feed, c.Path, c.WinnerSHA, c.LoserSHA).Scan(&exists); err != nil {
		return fmt.Errorf("check imported conflict: %w", err)
	}
	if !exists {
		if _, err := tx.Exec(ctx, `
			INSERT INTO publish_conflicts
				(feed, path, coordinate, winner_sha256, loser_sha256, winner_site, loser_site)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			c.Feed, c.Path, c.Coordinate, c.WinnerSHA, c.LoserSHA,
			c.WinnerSite, c.LoserSite); err != nil {
			return fmt.Errorf("import conflict: %w", err)
		}
	}
	return syncConflictQuarantineTx(ctx, tx, c.Feed, c.Coordinate)
}

// sideMeta is one side of a conflict: enough to restore a consistent row if
// an operator keeps this digest.
type sideMeta struct {
	SHA256    string            `json:"sha256"`
	Size      int64             `json:"size"`
	Checksums map[string]string `json:"checksums,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// recordConflictTx stores both sides of a K1 conflict, including each
// side's size and checksums.
func recordConflictTx(ctx context.Context, tx pgxTx, p ManifestPut,
	winner, loser sideMeta, winnerSite, loserSite string) error {
	winnerJSON, err := json.Marshal(winner)
	if err != nil {
		return fmt.Errorf("encode conflict winner: %w", err)
	}
	loserJSON, err := json.Marshal(loser)
	if err != nil {
		return fmt.Errorf("encode conflict loser: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO publish_conflicts
			(feed, path, coordinate, winner_sha256, loser_sha256, winner_site, loser_site,
			 winner_meta, loser_meta)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		p.Feed, p.Path, p.Coord, winner.SHA256, loser.SHA256, winnerSite, loserSite,
		winnerJSON, loserJSON)
	if err != nil {
		return fmt.Errorf("record conflict: %w", err)
	}
	return nil
}
