package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fondaco-dev/fondaco/core/state"
)

// quarantineCache answers "is this coordinate quarantined?" on the read path
// with a short TTL cache. When PostgreSQL is down the last known answer is
// reused, and unknown coordinates are treated as servable: the read path
// degrades, it does not fail (invariant 7).
// maxQuarantineEntries bounds the cache. It is a cache, not a ledger.
const maxQuarantineEntries = 8192

type quarantineCache struct {
	db     *state.DB // nil: quarantine disabled
	ttl    time.Duration
	logger *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	entries map[string]quarantineEntry
}

type quarantineEntry struct {
	blocked bool
	reason  string
	expires time.Time
}

func newQuarantineCache(db *state.DB, ttl time.Duration, logger *slog.Logger) *quarantineCache {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &quarantineCache{
		db:      db,
		ttl:     ttl,
		logger:  logger,
		now:     time.Now,
		entries: make(map[string]quarantineEntry),
	}
}

// Blocked reports whether the coordinate must not be served, with the
// human-readable reason.
func (q *quarantineCache) Blocked(ctx context.Context, feed, coordinate string) (bool, string) {
	if q == nil || q.db == nil {
		return false, ""
	}
	key := feed + "\x00" + coordinate

	q.mu.Lock()
	e, ok := q.entries[key]
	q.mu.Unlock()
	if ok && q.now().Before(e.expires) {
		return e.blocked, e.reason
	}

	entry, found, err := q.db.ActiveQuarantine(ctx, feed, coordinate)
	if err != nil {
		if ok {
			// Database down: keep serving the last known verdict.
			q.logger.Warn("quarantine lookup failed, using cached verdict",
				"feed", feed, "coordinate", coordinate, "error", err)
			return e.blocked, e.reason
		}
		q.logger.Warn("quarantine lookup failed, treating coordinate as servable",
			"feed", feed, "coordinate", coordinate, "error", err)
		return false, ""
	}

	next := quarantineEntry{expires: q.now().Add(q.ttl)}
	if found {
		next.blocked = true
		next.reason = entry.Reason
		if entry.Detail != "" {
			next.reason += ": " + entry.Detail
		}
	}
	q.mu.Lock()
	if len(q.entries) >= maxQuarantineEntries {
		// Every requested coordinate lands here, including ones that do not
		// exist, so a scan over unique paths would otherwise grow the
		// process without limit. Dropping the cache costs one database
		// round-trip per coordinate afterwards, never correctness.
		q.entries = make(map[string]quarantineEntry, maxQuarantineEntries)
	}
	q.entries[key] = next
	q.mu.Unlock()
	return next.blocked, next.reason
}
