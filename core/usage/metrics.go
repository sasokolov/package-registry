// Package usage answers "what is in this feed, and how much is it used" —
// for proxied feeds as much as hosted ones.
//
// A proxy is a cache, and a cache nobody can measure is one nobody can size.
// The awkward part is that the cache deliberately has no database rows: reads
// have to survive PostgreSQL being down (invariant 7), so what a feed has
// cached is knowable only by looking at the blob store. That is a scan, so it
// is periodic, and its result is stored — every replica then answers from the
// same numbers instead of each walking the store to render a page.
//
// Traffic is the opposite shape: cheap to observe, expensive to store one row
// at a time. It is counted in memory and flushed in batches. A request never
// waits for a counter, and a database outage costs the unflushed delta —
// these are usage numbers, and the audit log is where exactness lives.
//
// Nothing here is per package. A registry has an unbounded number of
// coordinates and a small number of feeds; labelling metrics by coordinate is
// how a Prometheus falls over. Feeds and groups are the units.
package usage

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sasokolov/package-registry/core/state"
)

// Metrics are the collectors for what feeds hold and serve.
type Metrics struct {
	// BytesServed is response bytes, by feed and where they came from.
	// Together with registry_requests_total it says both how often and how
	// much — a feed can be busy and tiny, or quiet and enormous.
	BytesServed *prometheus.CounterVec // feed, source
	// UpstreamBytes is what was pulled from an upstream to fill the cache.
	// The difference between this and BytesServed on a proxy feed is what
	// the cache saved.
	UpstreamBytes *prometheus.CounterVec // feed
	// GroupRequests counts what a group served and which member answered.
	// Without it a group is invisible: the pipeline only ever sees the
	// member, so "who is using the group URL" has no answer.
	GroupRequests *prometheus.CounterVec // group, member, source

	// Inventory, from the periodic scan.
	Artifacts  *prometheus.GaugeVec // feed, format, kind
	Packages   *prometheus.GaugeVec // feed, format, kind
	Bytes      *prometheus.GaugeVec // feed, format, kind
	LastIngest *prometheus.GaugeVec // feed
	// StoreBytes and StoreBlobs are the deduplicated totals: what the object
	// store holds, however many feeds point at it. Summing the per-feed
	// gauges instead would overstate it by exactly the sharing.
	StoreBytes prometheus.Gauge
	StoreBlobs prometheus.Gauge

	ScanDuration  prometheus.Histogram
	ScanFailures  prometheus.Counter
	FlushFailures prometheus.Counter
}

// Kinds of stored content, used as a metric label.
const (
	KindHosted = "hosted"
	KindCached = "cached"
)

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		BytesServed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_bytes_served_total",
			Help: "Response bytes served, by feed and response source.",
		}, []string{"feed", "source"}),
		UpstreamBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_upstream_bytes_total",
			Help: "Bytes pulled from upstreams to fill the cache, by feed.",
		}, []string{"feed"}),
		GroupRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_group_requests_total",
			Help: "Requests answered through a group, by group, answering member and source.",
		}, []string{"group", "member", "source"}),

		Artifacts: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_feed_artifacts",
			Help: "Stored artifacts per feed, by kind (hosted|cached).",
		}, []string{"feed", "format", "kind"}),
		Packages: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_feed_packages",
			Help: "Distinct package coordinates per feed, by kind (hosted|cached).",
		}, []string{"feed", "format", "kind"}),
		Bytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_feed_bytes",
			Help: "Bytes of blob storage a feed's content occupies, by kind. " +
				"Blobs are shared, so feeds can sum to more than the store holds.",
		}, []string{"feed", "format", "kind"}),
		LastIngest: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_feed_last_ingest_timestamp_seconds",
			Help: "When a feed last stored anything. A proxy feed that has gone quiet is " +
				"either unused or broken, and this is how the two are told apart.",
		}, []string{"feed"}),

		StoreBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_store_bytes",
			Help: "Bytes of blob storage in use, counting each blob once.",
		}),
		StoreBlobs: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_store_blobs",
			Help: "Distinct blobs stored.",
		}),
		ScanDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "registry_usage_scan_duration_seconds",
			Help:    "How long a full inventory scan took.",
			Buckets: []float64{1, 5, 15, 60, 300, 900, 3600},
		}),
		ScanFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "registry_usage_scan_failures_total",
			Help: "Inventory scans that could not complete.",
		}),
		FlushFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "registry_usage_flush_failures_total",
			Help: "Traffic-counter flushes that could not be written. Deltas are kept and retried.",
		}),
	}
	reg.MustRegister(m.BytesServed, m.UpstreamBytes, m.GroupRequests,
		m.Artifacts, m.Packages, m.Bytes, m.LastIngest, m.StoreBytes, m.StoreBlobs,
		m.ScanDuration, m.ScanFailures, m.FlushFailures)
	return m
}

// publish exports one feed's inventory as gauges.
func (m *Metrics) publish(feed, format string, u state.FeedUsage) {
	if m == nil {
		return
	}
	m.Artifacts.WithLabelValues(feed, format, KindHosted).Set(float64(u.HostedArtifacts))
	m.Artifacts.WithLabelValues(feed, format, KindCached).Set(float64(u.CachedArtifacts))
	m.Packages.WithLabelValues(feed, format, KindHosted).Set(float64(u.HostedPackages))
	m.Packages.WithLabelValues(feed, format, KindCached).Set(float64(u.CachedPackages))
	m.Bytes.WithLabelValues(feed, format, KindHosted).Set(float64(u.HostedBytes))
	m.Bytes.WithLabelValues(feed, format, KindCached).Set(float64(u.CachedBytes))
	if !u.LastIngestAt.IsZero() {
		m.LastIngest.WithLabelValues(feed).Set(float64(u.LastIngestAt.Unix()))
	}
}

// publishSite exports the deduplicated totals.
func (m *Metrics) publishSite(u state.SiteUsage) {
	if m == nil {
		return
	}
	m.StoreBytes.Set(float64(u.DistinctBytes))
	m.StoreBlobs.Set(float64(u.DistinctBlobs))
}

// forget drops the series of a feed that no longer exists, so a removed feed
// stops being reported forever.
func (m *Metrics) forget(feed, format string) {
	if m == nil {
		return
	}
	for _, kind := range []string{KindHosted, KindCached} {
		m.Artifacts.DeleteLabelValues(feed, format, kind)
		m.Packages.DeleteLabelValues(feed, format, kind)
		m.Bytes.DeleteLabelValues(feed, format, kind)
	}
	m.LastIngest.DeleteLabelValues(feed)
}
