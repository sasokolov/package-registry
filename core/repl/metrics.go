package repl

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the replication collectors. They answer the operational
// question invariant 16 demands: is any peer stream falling behind, and is
// anything diverging silently?
type Metrics struct {
	// Lag is head(origin) - applied(peer, origin): how many events this
	// site still has to apply.
	Lag *prometheus.GaugeVec // peer, origin
	// CursorAge is seconds since the last successful poll of a peer.
	CursorAge *prometheus.GaugeVec // peer
	// DurableLag is head - durable: the honest RPO (events whose blobs are
	// not local yet).
	DurableLag *prometheus.GaugeVec // peer, origin
	// Applied counts applied events by kind.
	Applied *prometheus.CounterVec // kind
	// Conflicts counts cross-site publish conflicts (rule K1).
	Conflicts prometheus.Counter
	// Parked is the number of events waiting for a retry.
	Parked prometheus.Gauge
	// PollFailures counts failed peer polls.
	PollFailures *prometheus.CounterVec // peer
	// FeedDigest exposes a per-feed digest of the hosted manifest set, so
	// divergence between sites is visible in monitoring, not silent.
	FeedDigest *prometheus.GaugeVec // feed
	// PeerFetches counts blobs fetched from peers by outcome.
	PeerFetches *prometheus.CounterVec // outcome
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Lag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_repl_lag",
			Help: "Journal entries this site has not applied yet, by peer and origin site.",
		}, []string{"peer", "origin"}),
		CursorAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_repl_cursor_age_seconds",
			Help: "Seconds since the last successful poll of a peer.",
		}, []string{"peer"}),
		DurableLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_repl_durable_lag",
			Help: "Applied entries whose blobs are not local yet (RPO), by peer and origin.",
		}, []string{"peer", "origin"}),
		Applied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_repl_applied_total",
			Help: "Replication events applied, by kind.",
		}, []string{"kind"}),
		Conflicts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "registry_repl_publish_conflicts_total",
			Help: "Cross-site publish conflicts resolved by rule K1.",
		}),
		Parked: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_repl_parked_events",
			Help: "Replication events parked for retry.",
		}),
		PollFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_repl_poll_failures_total",
			Help: "Failed peer polls, by peer.",
		}, []string{"peer"}),
		FeedDigest: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_repl_feed_digest",
			Help: "Numeric digest of a feed's hosted manifest set; sites that agree report the same value.",
		}, []string{"feed"}),
		PeerFetches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_repl_peer_blob_fetches_total",
			Help: "Blobs fetched from peers, by outcome.",
		}, []string{"outcome"}),
	}
	reg.MustRegister(m.Lag, m.CursorAge, m.DurableLag, m.Applied, m.Conflicts,
		m.Parked, m.PollFailures, m.FeedDigest, m.PeerFetches)
	return m
}

func (m *Metrics) applied(kind string) {
	if m != nil {
		m.Applied.WithLabelValues(kind).Inc()
	}
}

func (m *Metrics) conflict() {
	if m != nil {
		m.Conflicts.Inc()
	}
}

func (m *Metrics) parked() {
	if m != nil {
		m.Parked.Inc()
	}
}

func (m *Metrics) pollFailure(peer string) {
	if m != nil {
		m.PollFailures.WithLabelValues(peer).Inc()
	}
}

func (m *Metrics) peerFetch(outcome string) {
	if m != nil {
		m.PeerFetches.WithLabelValues(outcome).Inc()
	}
}
