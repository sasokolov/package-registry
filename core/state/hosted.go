package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	err = db.pool.QueryRow(ctx, insert,
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
	err = db.pool.QueryRow(ctx,
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
	_, err = db.pool.Exec(ctx, `
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

// ListHosted returns published coordinates of a feed under a path prefix,
// ordered by path (deterministic Reindex input).
func (db *DB) ListHosted(ctx context.Context, feed, prefix string) ([]HostedRow, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT feed, path, coordinate, sha256, size, checksums, metadata, mutable,
		       origin, site, published_by, published_at
		  FROM hosted_manifests
		 WHERE feed = $1 AND path LIKE $2 || '%'
		 ORDER BY path`, feed, prefix)
	if err != nil {
		return nil, classify(fmt.Errorf("list hosted manifests: %w", err))
	}
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
		ON CONFLICT ON CONSTRAINT quarantine_feed_coord_key
		DO UPDATE SET reason = EXCLUDED.reason, detail = EXCLUDED.detail, released_at = NULL`,
		feed, coordinate, reason, detail)
	if err != nil {
		return classify(fmt.Errorf("quarantine %s %s: %w", feed, coordinate, err))
	}
	return nil
}

// ReleaseQuarantine clears an active quarantine.
func (db *DB) ReleaseQuarantine(ctx context.Context, feed, coordinate string) error {
	_, err := db.pool.Exec(ctx,
		"UPDATE quarantine SET released_at = now() WHERE feed=$1 AND coordinate=$2 AND released_at IS NULL",
		feed, coordinate)
	if err != nil {
		return fmt.Errorf("release quarantine %s %s: %w", feed, coordinate, err)
	}
	return nil
}

// ActiveQuarantine returns the active quarantine record for a coordinate,
// or ok=false when it is servable.
func (db *DB) ActiveQuarantine(ctx context.Context, feed, coordinate string) (QuarantineEntry, bool, error) {
	var e QuarantineEntry
	err := db.pool.QueryRow(ctx, `
		SELECT feed, coordinate, reason, detail, created_at
		  FROM quarantine
		 WHERE feed=$1 AND coordinate=$2 AND released_at IS NULL`, feed, coordinate).
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
