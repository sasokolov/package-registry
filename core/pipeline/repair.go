package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sasokolov/package-registry/core/api"
)

// ProjectionRepair keeps the blob-store projection of hosted manifests in
// step with PostgreSQL, which is the source of truth. The projection is
// what the read path consults, so a missing or stale object would show up
// as a package that "exists" but cannot be downloaded — exactly the silent
// divergence invariant 16 forbids.
//
// It is a repair loop, not a write path: publishes and replication write
// the projection directly, and this only fixes what a crash or an S3
// outage left behind.
type ProjectionRepair struct {
	publisher *Publisher
	interval  time.Duration
	metrics   *RepairMetrics
}

// RepairMetrics exposes what the repair loop found.
type RepairMetrics struct {
	// Divergent is the number of hosted coordinates whose projection was
	// missing or wrong at the last pass. Anything but zero for longer than
	// one interval is an alert.
	Divergent prometheus.Gauge
	// Repaired counts projections rewritten.
	Repaired prometheus.Counter
	// Failures counts passes that could not complete.
	Failures prometheus.Counter
}

// NewRepairMetrics registers the repair collectors.
func NewRepairMetrics(reg prometheus.Registerer) *RepairMetrics {
	m := &RepairMetrics{
		Divergent: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_projection_divergent",
			Help: "Hosted coordinates whose blob-store projection disagrees with the database.",
		}),
		Repaired: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "registry_projection_repaired_total",
			Help: "Hosted manifest projections rewritten by the repair loop.",
		}),
		Failures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "registry_projection_repair_failures_total",
			Help: "Repair passes that could not complete.",
		}),
	}
	reg.MustRegister(m.Divergent, m.Repaired, m.Failures)
	return m
}

// NewProjectionRepair builds the loop.
func NewProjectionRepair(p *Publisher, interval time.Duration, metrics *RepairMetrics) *ProjectionRepair {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &ProjectionRepair{publisher: p, interval: interval, metrics: metrics}
}

// Run repairs on an interval until ctx is done.
func (r *ProjectionRepair) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if _, err := r.RepairOnce(ctx); err != nil {
			if r.metrics != nil {
				r.metrics.Failures.Inc()
			}
			r.publisher.logger.Warn("projection repair pass failed", "error", err)
		}
	}
}

// RepairOnce compares every hosted row with its projection and rewrites the
// ones that disagree. It returns how many it repaired.
func (r *ProjectionRepair) RepairOnce(ctx context.Context) (int, error) {
	if !r.publisher.Enabled() {
		return 0, nil
	}
	rows, err := r.publisher.db.ListHosted(ctx, "", "")
	if err != nil {
		return 0, fmt.Errorf("list hosted manifests: %w", err)
	}

	var divergent, repaired, failed int
	var firstErr error
	for _, row := range rows {
		ok, err := r.projectionMatches(ctx, row.Feed, row.Path, row.SHA256, row.Size)
		if err != nil {
			// One unreadable object must not blind the whole pass: the
			// gauge this loop feeds is what the divergence alert watches.
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ok {
			continue
		}
		divergent++

		// Re-read the row inside the write: this pass may have been
		// listing for a while, and a conflict resolution in the meantime
		// would otherwise be undone — writing back the digest an operator
		// just rejected.
		current, found, err := r.publisher.db.HostedRow(ctx, row.Feed, row.Path)
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !found || current.SHA256 != row.SHA256 {
			// The row changed under us; the next pass sees the new state.
			continue
		}
		if err := r.publisher.WriteReplicatedManifest(ctx, current.Feed, current.Path, current.SHA256,
			current.Size, current.Checksums, current.Metadata, current.Site, current.PublishedBy); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		repaired++
		r.publisher.logger.Info("hosted manifest projection repaired",
			"feed", current.Feed, "path", current.Path, "sha256", current.SHA256)
	}

	if r.metrics != nil {
		r.metrics.Divergent.Set(float64(divergent))
		r.metrics.Repaired.Add(float64(repaired))
	}
	if firstErr != nil {
		return repaired, fmt.Errorf("%d of %d coordinates could not be checked or repaired: %w",
			failed, len(rows), firstErr)
	}
	return repaired, nil
}

// projectionMatches reports whether the stored projection agrees with the
// database row. The size is compared too: a projection with the right
// digest and a wrong size serves a truncated response, and comparing
// digests alone would call that healthy.
func (r *ProjectionRepair) projectionMatches(ctx context.Context, feed, path, sha256hex string, size int64) (bool, error) {
	rc, _, err := r.publisher.store.Get(ctx, "manifests/"+feed+"/"+path)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("read projection %s/%s: %w", feed, path, err)
	}
	defer func() { _ = rc.Close() }()

	var m manifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		// Unreadable projection: rewrite it rather than guess.
		return false, nil
	}
	return m.SHA256 == sha256hex && m.Size == size, nil
}
