package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// HostedRow is one published coordinate.
type HostedRow struct {
	Feed        string
	Path        string
	Coordinate  string
	SHA256      string
	Size        int64
	Checksums   map[string]string
	Metadata    map[string]string
	Mutable     bool
	Origin      string
	Site        string
	PublishedBy string
	PublishedAt time.Time
}

// ErrAlreadyPublished is returned by InsertHosted when the coordinate
// exists with different content (invariant 4).
var ErrAlreadyPublished = errors.New("coordinate already published")

// InsertHosted records a published coordinate. Republishing identical
// content is idempotent (created=false); different content on an immutable
// coordinate yields ErrAlreadyPublished. Mutable coordinates are updated.
func (db *DB) InsertHosted(ctx context.Context, r HostedRow) (created bool, err error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return false, classify(fmt.Errorf("begin publish: %w", err))
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, err = InsertHostedTx(ctx, tx, r)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, classify(fmt.Errorf("commit publish: %w", err))
	}
	return created, nil
}

// InsertHostedTx is InsertHosted inside a caller-provided transaction, so a
// publish and its replication journal entry commit together.
func InsertHostedTx(ctx context.Context, tx pgx.Tx, r HostedRow) (created bool, err error) {
	// Stamp the row with the site's hybrid logical clock. Mutable
	// coordinates (dist-tags, SNAPSHOT aliases) are ordered by this stamp
	// when a replicated update arrives; an unstamped row would read as
	// (0,0) and lose to every remote event, however old.
	if r.Metadata == nil {
		r.Metadata = map[string]string{}
	}
	var wall, logical int64
	if err := tx.QueryRow(ctx, "SELECT hlc_wall, hlc_logical FROM repl_hlc_now()").Scan(&wall, &logical); err != nil {
		return false, classify(fmt.Errorf("stamp hybrid logical clock: %w", err))
	}
	r.Metadata["hlc_wall"] = strconv.FormatInt(wall, 10)
	r.Metadata["hlc_logical"] = strconv.FormatInt(logical, 10)

	checksums, err := json.Marshal(nonNilMap(r.Checksums))
	if err != nil {
		return false, fmt.Errorf("encode checksums: %w", err)
	}
	metadata, err := json.Marshal(nonNilMap(r.Metadata))
	if err != nil {
		return false, fmt.Errorf("encode metadata: %w", err)
	}
	if r.Origin == "" {
		r.Origin = "publish"
	}

	const insert = `
		INSERT INTO hosted_manifests
			(feed, path, coordinate, sha256, size, checksums, metadata, mutable, origin, site, published_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT ON CONSTRAINT hosted_manifests_feed_path_key DO NOTHING
		RETURNING sha256`
	var stored string
	err = tx.QueryRow(ctx, insert,
		r.Feed, r.Path, r.Coordinate, r.SHA256, r.Size, checksums, metadata,
		r.Mutable, r.Origin, r.Site, r.PublishedBy).Scan(&stored)
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return false, fmt.Errorf("insert hosted manifest: %s: %w", pgErr.Message, err)
		}
		return false, classify(fmt.Errorf("insert hosted manifest: %w", err))
	}

	// The row exists: decide between idempotent retry, mutable update and
	// an immutability violation.
	var existingSHA string
	var existingMutable bool
	err = tx.QueryRow(ctx,
		"SELECT sha256, mutable FROM hosted_manifests WHERE feed=$1 AND path=$2",
		r.Feed, r.Path).Scan(&existingSHA, &existingMutable)
	if err != nil {
		return false, classify(fmt.Errorf("read existing manifest: %w", err))
	}
	if existingSHA == r.SHA256 {
		return false, nil // identical content: idempotent
	}
	if !existingMutable || !r.Mutable {
		return false, fmt.Errorf("%s %s: %w", r.Feed, r.Path, ErrAlreadyPublished)
	}
	_, err = tx.Exec(ctx, `
		UPDATE hosted_manifests
		   SET sha256=$3, size=$4, checksums=$5, metadata=$6, published_by=$7,
		       site=$8, updated_at=now()
		 WHERE feed=$1 AND path=$2`,
		r.Feed, r.Path, r.SHA256, r.Size, checksums, metadata, r.PublishedBy, r.Site)
	if err != nil {
		return false, fmt.Errorf("update mutable manifest: %w", err)
	}
	return true, nil
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// HostedRow reads one coordinate, for callers that must act on the current
// value rather than one they listed earlier.
func (db *DB) HostedRow(ctx context.Context, feed, path string) (HostedRow, bool, error) {
	rows, err := db.pool.Query(ctx, listHostedQuery+" LIMIT 1", feed, path)
	if err != nil {
		return HostedRow{}, false, classify(fmt.Errorf("read hosted manifest: %w", err))
	}
	out, err := scanHosted(rows)
	if err != nil {
		return HostedRow{}, false, err
	}
	for _, r := range out {
		if r.Path == path {
			return r, true, nil
		}
	}
	return HostedRow{}, false, nil
}

// ActiveQuarantinesTx lists every coordinate currently blocked, for a
// bootstrap snapshot.
func ActiveQuarantinesTx(ctx context.Context, tx pgx.Tx) ([]QuarantineEntry, error) {
	rows, err := tx.Query(ctx, `
		SELECT feed, coordinate, reason, detail, created_at
		  FROM quarantine WHERE released_at IS NULL
		 ORDER BY feed, coordinate, reason`)
	if err != nil {
		return nil, fmt.Errorf("list active quarantines: %w", err)
	}
	defer rows.Close()
	var out []QuarantineEntry
	for rows.Next() {
		var e QuarantineEntry
		if err := rows.Scan(&e.Feed, &e.Coordinate, &e.Reason, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ResolutionRow is an operator's terminal decision for a coordinate.
type ResolutionRow struct {
	Feed       string
	Path       string
	Coordinate string
	KeepSHA    string
	Size       int64
	Checksums  map[string]string
	Metadata   map[string]string
	Operator   string
	HLCWall    int64
	HLCLogical int64
}

// ConflictResolutionsTx lists operator decisions for a bootstrap snapshot.
func ConflictResolutionsTx(ctx context.Context, tx pgx.Tx) ([]ResolutionRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT feed, path, coordinate, keep_sha256, size, checksums, metadata,
		       operator, hlc_wall, hlc_logical
		  FROM conflict_resolutions ORDER BY feed, path`)
	if err != nil {
		return nil, fmt.Errorf("list conflict resolutions: %w", err)
	}
	defer rows.Close()
	var out []ResolutionRow
	for rows.Next() {
		var r ResolutionRow
		var checksums, metadata []byte
		if err := rows.Scan(&r.Feed, &r.Path, &r.Coordinate, &r.KeepSHA, &r.Size,
			&checksums, &metadata, &r.Operator, &r.HLCWall, &r.HLCLogical); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(checksums, &r.Checksums)
		_ = json.Unmarshal(metadata, &r.Metadata)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokedTokenHashesTx lists revoked token hashes for a bootstrap snapshot.
// Only hashes travel — a secret never leaves the site that issued it
// (invariant 12).
func RevokedTokenHashesTx(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx,
		"SELECT hash FROM tokens WHERE revoked_at IS NOT NULL ORDER BY hash")
	if err != nil {
		return nil, fmt.Errorf("list revoked tokens: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListHosted returns published coordinates ordered by path (a deterministic
// Reindex input). An empty feed means every feed — snapshot, projection
// repair and backfill all need the whole set.
func (db *DB) ListHosted(ctx context.Context, feed, prefix string) ([]HostedRow, error) {
	rows, err := db.pool.Query(ctx, listHostedQuery, feed, prefix)
	if err != nil {
		return nil, classify(fmt.Errorf("list hosted manifests: %w", err))
	}
	return scanHosted(rows)
}

// ListHostedTx is ListHosted inside a caller's transaction, so a snapshot
// can read manifests and journal watermarks at one point in time.
func ListHostedTx(ctx context.Context, tx pgx.Tx, feed, prefix string) ([]HostedRow, error) {
	rows, err := tx.Query(ctx, listHostedQuery, feed, prefix)
	if err != nil {
		return nil, fmt.Errorf("list hosted manifests: %w", err)
	}
	return scanHosted(rows)
}

const listHostedQuery = `
		SELECT feed, path, coordinate, sha256, size, checksums, metadata, mutable,
		       origin, site, published_by, published_at
		  FROM hosted_manifests
		 WHERE ($1 = '' OR feed = $1) AND path LIKE $2 || '%'
		 ORDER BY feed, path`

// scanHosted materializes hosted rows from a query result.
func scanHosted(rows pgx.Rows) ([]HostedRow, error) {
	defer rows.Close()

	var out []HostedRow
	for rows.Next() {
		var r HostedRow
		var checksums, metadata []byte
		if err := rows.Scan(&r.Feed, &r.Path, &r.Coordinate, &r.SHA256, &r.Size,
			&checksums, &metadata, &r.Mutable, &r.Origin, &r.Site, &r.PublishedBy, &r.PublishedAt); err != nil {
			return nil, fmt.Errorf("scan hosted manifest: %w", err)
		}
		if err := json.Unmarshal(checksums, &r.Checksums); err != nil {
			return nil, fmt.Errorf("decode checksums: %w", err)
		}
		if err := json.Unmarshal(metadata, &r.Metadata); err != nil {
			return nil, fmt.Errorf("decode metadata: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Quarantine

// QuarantineEntry is an active quarantine record.
type QuarantineEntry struct {
	Feed       string
	Coordinate string
	Reason     string
	Detail     string
	CreatedAt  time.Time
}

// Quarantine marks a coordinate as not servable until released.
func (db *DB) Quarantine(ctx context.Context, feed, coordinate, reason, detail string) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO quarantine (feed, coordinate, reason, detail)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT ON CONSTRAINT quarantine_feed_coord_reason_key
		DO UPDATE SET detail = EXCLUDED.detail, released_at = NULL`,
		feed, coordinate, reason, detail)
	if err != nil {
		return classify(fmt.Errorf("quarantine %s %s: %w", feed, coordinate, err))
	}
	return nil
}

// ReleaseQuarantine clears active quarantines of a coordinate. An empty
// reason releases every reason; naming one releases just that reason, so a
// resolved conflict does not lift an unrelated manual takedown.
func (db *DB) ReleaseQuarantine(ctx context.Context, feed, coordinate, reason string) error {
	query := "UPDATE quarantine SET released_at = now() WHERE feed=$1 AND coordinate=$2 AND released_at IS NULL"
	args := []any{feed, coordinate}
	if reason != "" {
		query += " AND reason = $3"
		args = append(args, reason)
	}
	_, err := db.pool.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("release quarantine %s %s: %w", feed, coordinate, err)
	}
	return nil
}

// ActiveQuarantine returns the active quarantine record for a coordinate,
// or ok=false when it is servable.
func (db *DB) ActiveQuarantine(ctx context.Context, feed, coordinate string) (QuarantineEntry, bool, error) {
	var e QuarantineEntry
	// Any active reason blocks the coordinate; the oldest one is reported.
	err := db.pool.QueryRow(ctx, `
		SELECT feed, coordinate, reason, detail, created_at
		  FROM quarantine
		 WHERE feed=$1 AND coordinate=$2 AND released_at IS NULL
		 ORDER BY created_at
		 LIMIT 1`, feed, coordinate).
		Scan(&e.Feed, &e.Coordinate, &e.Reason, &e.Detail, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return QuarantineEntry{}, false, nil
	}
	if err != nil {
		return QuarantineEntry{}, false, classify(fmt.Errorf("read quarantine: %w", err))
	}
	return e, true, nil
}

// ---------------------------------------------------------------------------
// Policy verdict cache (generic: the core knows no policy specifics)

// GetVerdict reads a cached policy verdict; ok=false when absent.
func (db *DB) GetVerdict(ctx context.Context, namespace, key string) (value string, checkedAt time.Time, ok bool, err error) {
	err = db.pool.QueryRow(ctx,
		"SELECT value, checked_at FROM policy_verdicts WHERE namespace=$1 AND key=$2",
		namespace, key).Scan(&value, &checkedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("read policy verdict: %w", err)
	}
	return value, checkedAt, true, nil
}

// PutVerdict stores or refreshes a policy verdict.
func (db *DB) PutVerdict(ctx context.Context, namespace, key, value string) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO policy_verdicts (namespace, key, value, checked_at)
		VALUES ($1,$2,$3, now())
		ON CONFLICT (namespace, key)
		DO UPDATE SET value = EXCLUDED.value, checked_at = now()`,
		namespace, key, value)
	if err != nil {
		return fmt.Errorf("store policy verdict: %w", err)
	}
	return nil
}
