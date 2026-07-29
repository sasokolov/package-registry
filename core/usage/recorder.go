package usage

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sasokolov/package-registry/core/state"
)

// Recorder counts what feeds served, in memory, and folds the totals into
// PostgreSQL periodically.
//
// In memory on purpose. A download is the hottest path this registry has, and
// a counter that writes a row per request would make the database part of
// serving — exactly what invariant 7 says it must not be. What is lost if a
// replica is killed is the delta since the last flush: a number on a screen,
// not a fact anybody relies on. Prometheus gets the same events immediately
// and is where rates belong; this is for the cumulative "downloaded N times"
// that has to survive a restart.
type Recorder struct {
	metrics *Metrics
	logger  *slog.Logger

	mu    sync.Mutex
	feeds map[key]*counter
}

type key struct {
	feed   string
	source string
}

type counter struct {
	requests int64
	bytes    int64
}

// SourceIngest is the pseudo-source under which bytes pulled from an upstream
// are counted. It is not a way a request was answered — it is what answering
// cost — and keeping it in the same table is what makes "how much did the
// cache save" a single subtraction.
const SourceIngest = "ingest"

// SourceMerged is the source of a document a group assembled from several
// members. It is the group's own answer — no single member produced it — so
// it is the one kind of group traffic that is not also counted on a member,
// and the one a site total has to add separately.
const SourceMerged = "merged"

// NewRecorder builds a recorder. Metrics may be nil in tests.
func NewRecorder(metrics *Metrics, logger *slog.Logger) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{metrics: metrics, logger: logger, feeds: map[key]*counter{}}
}

// Served records one response. Size may be negative when the body's length
// was not known, in which case only the request is counted: a guessed byte
// count is worse than an honest gap.
func (r *Recorder) Served(feed, source string, size int64) {
	if r == nil || feed == "" {
		return
	}
	if size < 0 {
		size = 0
	}
	if r.metrics != nil && size > 0 {
		r.metrics.BytesServed.WithLabelValues(feed, source).Add(float64(size))
	}
	r.add(key{feed, source}, 1, size)
}

// Ingested records bytes pulled from an upstream to fill the cache.
func (r *Recorder) Ingested(feed string, size int64) {
	if r == nil || feed == "" || size <= 0 {
		return
	}
	if r.metrics != nil {
		r.metrics.UpstreamBytes.WithLabelValues(feed).Add(float64(size))
	}
	r.add(key{feed, SourceIngest}, 1, size)
}

// GroupServed records that a group answered, and which member did the work.
//
// The member is counted separately, under its own name, so "how much is this
// feed used" includes what arrived through a group. This row is the other
// question — what the group URL itself was asked for — and the two have
// different fixes: nobody using the group URL is a client-configuration
// problem, and an empty member is a content one.
func (r *Recorder) GroupServed(group, member, source string, size int64) {
	if r == nil || group == "" {
		return
	}
	if size < 0 {
		size = 0
	}
	if r.metrics != nil {
		r.metrics.GroupRequests.WithLabelValues(group, member, source).Inc()
		if size > 0 {
			r.metrics.BytesServed.WithLabelValues(group, source).Add(float64(size))
		}
	}
	r.add(key{group, source}, 1, size)
}

func (r *Recorder) add(k key, requests, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.feeds[k]
	if c == nil {
		c = &counter{}
		r.feeds[k] = c
	}
	c.requests += requests
	c.bytes += bytes
}

// take removes and returns the accumulated deltas.
func (r *Recorder) take() []state.FeedTraffic {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.feeds) == 0 {
		return nil
	}
	out := make([]state.FeedTraffic, 0, len(r.feeds))
	for k, c := range r.feeds {
		out = append(out, state.FeedTraffic{
			Feed: k.feed, Source: k.source, Requests: c.requests, Bytes: c.bytes,
		})
	}
	r.feeds = map[key]*counter{}
	return out
}

// restore puts deltas back after a failed flush, folding them into whatever
// arrived meanwhile. Dropping them would make an outage look like idleness.
func (r *Recorder) restore(deltas []state.FeedTraffic) {
	for _, d := range deltas {
		r.add(key{d.Feed, d.Source}, d.Requests, d.Bytes)
	}
}

// Flush writes the accumulated deltas. On failure they are kept for the next
// attempt rather than discarded.
func (r *Recorder) Flush(ctx context.Context, db *state.DB) error {
	if r == nil || db == nil {
		return nil
	}
	deltas := r.take()
	if len(deltas) == 0 {
		return nil
	}
	if err := db.AddTraffic(ctx, deltas); err != nil {
		r.restore(deltas)
		if r.metrics != nil {
			r.metrics.FlushFailures.Inc()
		}
		return err
	}
	return nil
}

// Run flushes on an interval until the context is cancelled, then once more
// so a graceful shutdown does not throw away the last minute.
func (r *Recorder) Run(ctx context.Context, db *state.DB, interval time.Duration) {
	if r == nil || db == nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// A cancelled context cannot be used to write; the deadline is
			// short because shutdown should not wait on a sick database.
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := r.Flush(final, db); err != nil {
				r.logger.Warn("final usage flush failed; counters are short by one interval",
					"error", err)
			}
			cancel()
			return
		case <-ticker.C:
			if err := r.Flush(ctx, db); err != nil {
				r.logger.Warn("usage flush failed; counters will be retried", "error", err)
			}
		}
	}
}
