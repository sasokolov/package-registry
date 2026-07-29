package state

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Feed usage: what a feed holds, and how much it is used.
//
// Everything here is derived. A failed write costs a number on a screen and
// nothing else, so callers log and carry on rather than failing a request
// (invariant 7).

// FeedUsage is one feed's inventory, as the last scan found it.
type FeedUsage struct {
	Feed            string
	HostedArtifacts int64
	CachedArtifacts int64
	HostedPackages  int64
	CachedPackages  int64
	HostedBytes     int64
	CachedBytes     int64
	// SharedBytes is the part of Bytes that another feed also points at.
	// Blobs are content-addressed, so the same tarball proxied by two feeds
	// is stored once: this is the difference between what a feed costs and
	// what deleting it would free.
	SharedBytes  int64
	LastIngestAt time.Time
	ScannedAt    time.Time
}

// Artifacts is everything this feed stores.
func (u FeedUsage) Artifacts() int64 { return u.HostedArtifacts + u.CachedArtifacts }

// Packages is the distinct coordinates it holds. A feed either hosts or
// proxies, so the two halves cannot double-count each other.
func (u FeedUsage) Packages() int64 { return u.HostedPackages + u.CachedPackages }

// Bytes is what this feed's content occupies in the blob store.
func (u FeedUsage) Bytes() int64 { return u.HostedBytes + u.CachedBytes }

// SiteUsage is what the object store holds in total, counting each blob once.
type SiteUsage struct {
	DistinctBlobs int64
	DistinctBytes int64
	ScannedAt     time.Time
}

// SaveSiteUsage records the deduplicated total.
func (db *DB) SaveSiteUsage(ctx context.Context, u SiteUsage) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO usage_site (only_row, distinct_blobs, distinct_bytes, scanned_at)
		VALUES (true, $1, $2, now())
		ON CONFLICT (only_row) DO UPDATE SET
		    distinct_blobs = EXCLUDED.distinct_blobs,
		    distinct_bytes = EXCLUDED.distinct_bytes,
		    scanned_at = now()`, u.DistinctBlobs, u.DistinctBytes)
	if err != nil {
		return classify(fmt.Errorf("save site usage: %w", err))
	}
	return nil
}

// SiteUsage returns the deduplicated total, or a zero value when no scan has
// run yet.
func (db *DB) SiteUsage(ctx context.Context) (SiteUsage, error) {
	var u SiteUsage
	err := db.pool.QueryRow(ctx, `
		SELECT distinct_blobs, distinct_bytes, scanned_at FROM usage_site`).
		Scan(&u.DistinctBlobs, &u.DistinctBytes, &u.ScannedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SiteUsage{}, nil
		}
		return SiteUsage{}, classify(fmt.Errorf("read site usage: %w", err))
	}
	return u, nil
}

// FeedTraffic is one feed's counters for one response source.
type FeedTraffic struct {
	Feed     string
	Source   string
	Requests int64
	Bytes    int64
	LastAt   time.Time
}

// SaveUsage replaces the inventory for the feeds a scan covered.
//
// Feeds absent from the scan are left alone: a scan that skipped a feed
// because its store listing failed must not report it as empty.
func (db *DB) SaveUsage(ctx context.Context, rows []FeedUsage) error {
	if len(rows) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		var lastIngest any
		if !r.LastIngestAt.IsZero() {
			lastIngest = r.LastIngestAt
		}
		batch.Queue(`
			INSERT INTO feed_usage (feed, hosted_artifacts, cached_artifacts,
			                        hosted_packages, cached_packages,
			                        hosted_bytes, cached_bytes, shared_bytes,
			                        last_ingest_at, scanned_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, now())
			ON CONFLICT (feed) DO UPDATE SET
			    hosted_artifacts = EXCLUDED.hosted_artifacts,
			    cached_artifacts = EXCLUDED.cached_artifacts,
			    hosted_packages = EXCLUDED.hosted_packages,
			    cached_packages = EXCLUDED.cached_packages,
			    hosted_bytes = EXCLUDED.hosted_bytes,
			    cached_bytes = EXCLUDED.cached_bytes,
			    shared_bytes = EXCLUDED.shared_bytes,
			    last_ingest_at = EXCLUDED.last_ingest_at,
			    scanned_at = now()`,
			r.Feed, r.HostedArtifacts, r.CachedArtifacts,
			r.HostedPackages, r.CachedPackages,
			r.HostedBytes, r.CachedBytes, r.SharedBytes, lastIngest)
	}
	if err := db.pool.SendBatch(ctx, batch).Close(); err != nil {
		return classify(fmt.Errorf("save feed usage: %w", err))
	}
	return nil
}

// ForgetUsage drops rows for feeds that no longer exist, so a removed feed
// stops showing up in totals.
func (db *DB) ForgetUsage(ctx context.Context, keep []string) error {
	if _, err := db.pool.Exec(ctx,
		`DELETE FROM feed_usage WHERE NOT (feed = ANY($1))`, keep); err != nil {
		return classify(fmt.Errorf("forget feed usage: %w", err))
	}
	if _, err := db.pool.Exec(ctx,
		`DELETE FROM feed_traffic WHERE NOT (feed = ANY($1))`, keep); err != nil {
		return classify(fmt.Errorf("forget feed traffic: %w", err))
	}
	return nil
}

// Usage returns the inventory of every feed the last scan covered.
func (db *DB) Usage(ctx context.Context) ([]FeedUsage, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT feed, hosted_artifacts, cached_artifacts,
		       hosted_packages, cached_packages,
		       hosted_bytes, cached_bytes, shared_bytes,
		       COALESCE(last_ingest_at, 'epoch'::timestamptz), scanned_at
		  FROM feed_usage ORDER BY feed`)
	if err != nil {
		return nil, classify(fmt.Errorf("read feed usage: %w", err))
	}
	defer rows.Close()

	var out []FeedUsage
	for rows.Next() {
		var u FeedUsage
		if err := rows.Scan(&u.Feed,
			&u.HostedArtifacts, &u.CachedArtifacts,
			&u.HostedPackages, &u.CachedPackages,
			&u.HostedBytes, &u.CachedBytes, &u.SharedBytes,
			&u.LastIngestAt, &u.ScannedAt); err != nil {
			return nil, classify(fmt.Errorf("scan feed usage: %w", err))
		}
		if u.LastIngestAt.Unix() <= 0 {
			u.LastIngestAt = time.Time{}
		}
		out = append(out, u)
	}
	return out, classify(rows.Err())
}

// AddTraffic folds a batch of counter deltas into the stored totals.
//
// Deltas, not absolutes: several replicas serve the same feed, and each one
// knows only what it served. Adding is the only operation that composes.
func (db *DB) AddTraffic(ctx context.Context, deltas []FeedTraffic) error {
	if len(deltas) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, d := range deltas {
		batch.Queue(`
			INSERT INTO feed_traffic (feed, source, requests, bytes, last_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (feed, source) DO UPDATE SET
			    requests = feed_traffic.requests + EXCLUDED.requests,
			    bytes = feed_traffic.bytes + EXCLUDED.bytes,
			    last_at = now()`,
			d.Feed, d.Source, d.Requests, d.Bytes)
	}
	if err := db.pool.SendBatch(ctx, batch).Close(); err != nil {
		return classify(fmt.Errorf("add feed traffic: %w", err))
	}
	return nil
}

// Traffic returns every feed's counters, one row per source.
func (db *DB) Traffic(ctx context.Context) ([]FeedTraffic, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT feed, source, requests, bytes, last_at
		  FROM feed_traffic ORDER BY feed, source`)
	if err != nil {
		return nil, classify(fmt.Errorf("read feed traffic: %w", err))
	}
	defer rows.Close()

	var out []FeedTraffic
	for rows.Next() {
		var t FeedTraffic
		if err := rows.Scan(&t.Feed, &t.Source, &t.Requests, &t.Bytes, &t.LastAt); err != nil {
			return nil, classify(fmt.Errorf("scan feed traffic: %w", err))
		}
		out = append(out, t)
	}
	return out, classify(rows.Err())
}

// PackageDownload is one coordinate's counters within one feed.
type PackageDownload struct {
	Feed       string
	Coordinate string
	Downloads  int64
	Bytes      int64
	LastAt     time.Time
}

// AddPackageDownloads folds a batch of per-coordinate deltas into the totals.
func (db *DB) AddPackageDownloads(ctx context.Context, deltas []PackageDownload) error {
	if len(deltas) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, d := range deltas {
		batch.Queue(`
			INSERT INTO package_downloads (feed, coordinate, downloads, bytes, last_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (feed, coordinate) DO UPDATE SET
			    downloads = package_downloads.downloads + EXCLUDED.downloads,
			    bytes = package_downloads.bytes + EXCLUDED.bytes,
			    last_at = now()`,
			d.Feed, d.Coordinate, d.Downloads, d.Bytes)
	}
	if err := db.pool.SendBatch(ctx, batch).Close(); err != nil {
		return classify(fmt.Errorf("add package downloads: %w", err))
	}
	return nil
}

// TopPackages returns the most downloaded coordinates, for one feed or across
// all of them. Across feeds the same coordinate in two feeds stays two rows:
// they are different objects with different access rules, and merging them
// would hide which feed is doing the work.
func (db *DB) TopPackages(ctx context.Context, feed string, limit int) ([]PackageDownload, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.pool.Query(ctx, `
		SELECT feed, coordinate, downloads, bytes, last_at
		  FROM package_downloads
		 WHERE ($1 = '' OR feed = $1)
		 ORDER BY downloads DESC, last_at DESC
		 LIMIT $2`, feed, limit)
	if err != nil {
		return nil, classify(fmt.Errorf("read top packages: %w", err))
	}
	defer rows.Close()

	var out []PackageDownload
	for rows.Next() {
		var p PackageDownload
		if err := rows.Scan(&p.Feed, &p.Coordinate, &p.Downloads, &p.Bytes, &p.LastAt); err != nil {
			return nil, classify(fmt.Errorf("scan top packages: %w", err))
		}
		out = append(out, p)
	}
	return out, classify(rows.Err())
}

// PrunePackageDownloads keeps each feed's most downloaded coordinates and
// drops the tail, so the table stays bounded on a proxy that has seen a
// million coordinates go by.
//
// The tail is what nobody is asking about: a coordinate outside its feed's
// top keep is by definition not in a top-N list. Anything downloaded
// recently is kept regardless, so something on its way up is not repeatedly
// knocked back to zero.
func (db *DB) PrunePackageDownloads(ctx context.Context, keepPerFeed int, active time.Duration) (int64, error) {
	if keepPerFeed <= 0 {
		return 0, nil
	}
	tag, err := db.pool.Exec(ctx, `
		DELETE FROM package_downloads p
		 WHERE p.last_at < now() - $2::interval
		   AND p.ctid NOT IN (
		       SELECT ctid FROM (
		           SELECT ctid, row_number() OVER (PARTITION BY feed ORDER BY downloads DESC) AS rank
		             FROM package_downloads
		       ) ranked WHERE rank <= $1)`,
		keepPerFeed, active.String())
	if err != nil {
		return 0, classify(fmt.Errorf("prune package downloads: %w", err))
	}
	return tag.RowsAffected(), nil
}

// ForgetPackageDownloads drops rows for feeds that no longer exist.
func (db *DB) ForgetPackageDownloads(ctx context.Context, keep []string) error {
	if _, err := db.pool.Exec(ctx,
		`DELETE FROM package_downloads WHERE NOT (feed = ANY($1))`, keep); err != nil {
		return classify(fmt.Errorf("forget package downloads: %w", err))
	}
	return nil
}
