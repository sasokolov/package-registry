package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/fondaco-dev/fondaco/core/api"
	"github.com/fondaco-dev/fondaco/core/state"
)

// Scanner works out what each feed holds.
//
// Hosted content is counted from the database, which is its source of truth
// and answers in one query. Proxy-cached content has no rows anywhere — that
// is deliberate, so downloads survive a database outage — so it is counted by
// walking manifests/<feed>/ and reading each manifest for the size and
// coordinate it points at.
//
// The walk skips manifests the database already accounted for, so a
// hosted-only site does no reads at all, and a feed that both hosts and
// proxies is not counted twice.
type Scanner struct {
	store   api.BlobStore
	db      *state.DB
	metrics *Metrics
	logger  *slog.Logger
	feeds   func() []Feed
	// lock serializes scans across replicas. Several replicas walking the
	// same store would each pay for it and write the same answer.
	lock func(ctx context.Context, key string, fn func(context.Context) error) error
}

// Feed is what the scanner needs to know about a configured feed.
type Feed struct {
	Name   string
	Format string
	Group  bool
}

// Options configure a Scanner.
type Options struct {
	Store   api.BlobStore
	DB      *state.DB
	Metrics *Metrics
	Logger  *slog.Logger
	// Feeds reports the feeds in force. It is a function because a
	// configuration reload replaces them.
	Feeds func() []Feed
	Lock  func(ctx context.Context, key string, fn func(context.Context) error) error
}

// NewScanner builds a scanner.
func NewScanner(o Options) *Scanner {
	s := &Scanner{
		store: o.Store, db: o.DB, metrics: o.Metrics,
		logger: o.Logger, feeds: o.Feeds, lock: o.Lock,
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.lock == nil {
		s.lock = func(ctx context.Context, _ string, fn func(context.Context) error) error {
			return fn(ctx)
		}
	}
	return s
}

// storedManifest is the subset of a manifest the scan reads. It is declared
// here rather than shared with core/pipeline so that adding a field there
// cannot silently change what this counts.
type storedManifest struct {
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	Coordinate string    `json:"coordinate,omitempty"`
	IngestedAt time.Time `json:"ingested_at"`
	Origin     string    `json:"origin,omitempty"`
}

// Report is what one scan found.
type Report struct {
	// Feeds is one row per non-group feed.
	Feeds []state.FeedUsage
	// Site is the deduplicated total: what the object store holds, counting
	// each blob once however many feeds point at it.
	Site state.SiteUsage
}

// ScanOnce computes the inventory and stores it.
func (s *Scanner) ScanOnce(ctx context.Context) (Report, error) {
	started := time.Now()
	var report Report
	err := s.lock(ctx, "usage-scan", func(ctx context.Context) error {
		var err error
		report.Feeds, report.Site, err = s.compute(ctx)
		if err != nil {
			return err
		}
		if s.db == nil {
			return nil
		}
		if err := s.db.SaveUsage(ctx, report.Feeds); err != nil {
			return fmt.Errorf("store inventory: %w", err)
		}
		if err := s.db.SaveSiteUsage(ctx, report.Site); err != nil {
			return fmt.Errorf("store site total: %w", err)
		}
		// Every configured feed, not only the ones with an inventory: a
		// group has no storage of its own and so no row in the report, but
		// it does have traffic, and dropping it here would reset the
		// counters of every group on every scan.
		configured := s.feeds()
		names := make([]string, 0, len(configured))
		for _, f := range configured {
			names = append(names, f.Name)
		}
		if err := s.db.ForgetUsage(ctx, names); err != nil {
			// A stale row for a deleted feed is untidy, not wrong.
			s.logger.Warn("could not drop usage of removed feeds", "error", err)
		}
		if err := s.db.ForgetPackageDownloads(ctx, names); err != nil {
			s.logger.Warn("could not drop package downloads of removed feeds", "error", err)
		}
		// The leaderboard's tail is nobody's question. Trimming it here —
		// under the same lock, on the same schedule — is what keeps the
		// table bounded on a proxy that has seen a million coordinates.
		if dropped, err := s.db.PrunePackageDownloads(ctx, keepPerFeed, keepActiveFor); err != nil {
			s.logger.Warn("could not prune package downloads", "error", err)
		} else if dropped > 0 {
			s.logger.Info("pruned the tail of the download leaderboard",
				"rows", dropped, "kept_per_feed", keepPerFeed)
		}
		return nil
	})
	if err != nil {
		if s.metrics != nil {
			s.metrics.ScanFailures.Inc()
		}
		return Report{}, err
	}
	if s.metrics != nil {
		s.metrics.ScanDuration.Observe(time.Since(started).Seconds())
	}
	return report, nil
}

// compute walks the database and the store and builds one row per feed.
func (s *Scanner) compute(ctx context.Context) ([]state.FeedUsage, state.SiteUsage, error) {
	feeds := s.feeds()
	tally := map[string]*feedTally{}
	byFormat := map[string]string{}
	for _, f := range feeds {
		byFormat[f.Name] = f.Format
		if f.Group {
			// A group stores nothing of its own; its numbers are its
			// members', and adding them here would count them twice.
			continue
		}
		tally[f.Name] = newTally()
	}

	// blobs remembers which feeds point at each digest, so the part of a
	// feed's bytes that another feed also holds can be told apart from the
	// part only it holds. One entry per distinct blob, which is the same
	// order the garbage collector already works in.
	blobs := map[string]*blobRef{}

	hostedPaths, err := s.countHosted(ctx, tally, blobs)
	if err != nil {
		return nil, state.SiteUsage{}, err
	}
	if err := s.countCached(ctx, tally, blobs, hostedPaths); err != nil {
		return nil, state.SiteUsage{}, err
	}

	// Attribute shared bytes, and total the distinct ones. Per-feed bytes
	// answer "what does this feed cost"; the site total answers "what is
	// the bill", and they differ by exactly the sharing.
	site := state.SiteUsage{DistinctBlobs: int64(len(blobs))}
	for _, ref := range blobs {
		site.DistinctBytes += ref.size
		if len(ref.feeds) < 2 {
			continue
		}
		for feed := range ref.feeds {
			if t := tally[feed]; t != nil {
				t.shared += ref.size
			}
		}
	}

	out := make([]state.FeedUsage, 0, len(tally))
	for name, t := range tally {
		u := t.usage(name)
		out = append(out, u)
		s.metrics.publish(name, byFormat[name], u)
	}
	s.metrics.publishSite(site)
	return out, site, nil
}

type blobRef struct {
	size  int64
	feeds map[string]bool
}

type feedTally struct {
	hostedArtifacts int64
	cachedArtifacts int64
	hostedBytes     int64
	cachedBytes     int64
	shared          int64
	hostedCoords    map[string]bool
	cachedCoords    map[string]bool
	lastIngest      time.Time
}

func newTally() *feedTally {
	return &feedTally{hostedCoords: map[string]bool{}, cachedCoords: map[string]bool{}}
}

func (t *feedTally) usage(feed string) state.FeedUsage {
	return state.FeedUsage{
		Feed:            feed,
		HostedArtifacts: t.hostedArtifacts,
		CachedArtifacts: t.cachedArtifacts,
		HostedPackages:  int64(len(t.hostedCoords)),
		CachedPackages:  int64(len(t.cachedCoords)),
		HostedBytes:     t.hostedBytes,
		CachedBytes:     t.cachedBytes,
		SharedBytes:     t.shared,
		LastIngestAt:    t.lastIngest,
	}
}

func (t *feedTally) seen(at time.Time) {
	if at.After(t.lastIngest) {
		t.lastIngest = at
	}
}

func reference(blobs map[string]*blobRef, sha string, size int64, feed string) {
	if sha == "" {
		return
	}
	ref := blobs[sha]
	if ref == nil {
		ref = &blobRef{size: size, feeds: map[string]bool{}}
		blobs[sha] = ref
	}
	ref.feeds[feed] = true
}

// countHosted folds the published coordinates in, and returns the manifest
// keys they own so the store walk can skip them.
func (s *Scanner) countHosted(ctx context.Context, tally map[string]*feedTally,
	blobs map[string]*blobRef) (map[string]bool, error) {
	owned := map[string]bool{}
	if s.db == nil {
		return owned, nil
	}
	rows, err := s.db.ListHosted(ctx, "", "")
	if err != nil {
		return nil, fmt.Errorf("list hosted manifests: %w", err)
	}
	for _, row := range rows {
		owned[row.Feed+"/"+row.Path] = true
		t := tally[row.Feed]
		if t == nil {
			// A feed that was removed from the configuration but whose rows
			// are still around. Counting it would resurrect it on screens.
			continue
		}
		t.hostedArtifacts++
		t.hostedBytes += row.Size
		if row.Coordinate != "" {
			t.hostedCoords[row.Coordinate] = true
		}
		t.seen(row.PublishedAt)
		reference(blobs, row.SHA256, row.Size, row.Feed)
	}
	return owned, nil
}

// countCached walks the manifests the cache wrote.
func (s *Scanner) countCached(ctx context.Context, tally map[string]*feedTally,
	blobs map[string]*blobRef, hosted map[string]bool) error {
	iter, err := s.store.List(ctx, "manifests/")
	if err != nil {
		return fmt.Errorf("list manifests: %w", err)
	}
	for {
		info, ok := iter.Next(ctx)
		if !ok {
			break
		}
		rest := strings.TrimPrefix(info.Key, "manifests/")
		feed, path, found := strings.Cut(rest, "/")
		if !found || hosted[feed+"/"+path] {
			continue
		}
		t := tally[feed]
		if t == nil {
			continue
		}

		m, err := s.readManifest(ctx, info.Key)
		if err != nil {
			// One unreadable manifest is a gap in a number, not a reason to
			// abandon the pass. It is logged so a store that is failing
			// wholesale is visible rather than quietly undercounting.
			s.logger.Warn("usage scan skipped an unreadable manifest",
				"key", info.Key, "error", err)
			continue
		}
		if m.Origin == originPublish {
			// A published manifest whose row is gone — a database restored
			// from an older backup, say. It is hosted content either way.
			t.hostedArtifacts++
			t.hostedBytes += m.Size
			if m.Coordinate != "" {
				t.hostedCoords[m.Coordinate] = true
			}
		} else {
			t.cachedArtifacts++
			t.cachedBytes += m.Size
			if m.Coordinate != "" {
				t.cachedCoords[m.Coordinate] = true
			}
		}
		t.seen(m.IngestedAt)
		reference(blobs, m.SHA256, m.Size, feed)
	}
	return iter.Err()
}

// originPublish marks a manifest written by a publish rather than an ingest.
const originPublish = "publish"

func (s *Scanner) readManifest(ctx context.Context, key string) (storedManifest, error) {
	rc, _, err := s.store.Get(ctx, key)
	if err != nil {
		return storedManifest{}, err
	}
	defer func() { _ = rc.Close() }()

	var m storedManifest
	// Manifests are small; the limit is a guard against a truncated or
	// wrong object, not a real bound.
	if err := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&m); err != nil {
		return storedManifest{}, err
	}
	return m, nil
}

// How much of the download leaderboard is worth keeping.
//
// A coordinate outside its feed's top thousand is, by definition, not in a
// top-N list; keeping it would only grow the table. Anything downloaded
// recently is kept regardless, so something on its way up is not repeatedly
// knocked back to zero before it gets there.
const (
	keepPerFeed   = 1000
	keepActiveFor = 30 * 24 * time.Hour
)

// retryAfterFailure bounds how long a failed pass waits. The interval is
// chosen for how often the numbers need refreshing, which on a large store is
// hours; a first pass that raced the schema migration must not leave the
// console empty for that long.
const retryAfterFailure = time.Minute

// Run scans on an interval, starting with one pass so a fresh replica does
// not show empty numbers until the first tick.
func (s *Scanner) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	for {
		wait := interval
		if _, err := s.ScanOnce(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("usage scan failed", "error", err, "retry_in", retryAfterFailure)
			wait = min(interval, retryAfterFailure)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Forget drops the exported series of feeds that are no longer configured.
func (s *Scanner) Forget(feed, format string) { s.metrics.forget(feed, format) }
