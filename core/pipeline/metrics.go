package pipeline

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the pipeline's Prometheus collectors. RPS per feed and the
// cache hit ratio derive from registry_requests_total (labels feed, source).
type Metrics struct {
	Requests         *prometheus.CounterVec   // feed, source
	Failures         *prometheus.CounterVec   // feed, reason
	UpstreamRequests *prometheus.CounterVec   // feed, outcome
	UpstreamDuration *prometheus.HistogramVec // feed
	BreakerState     *prometheus.GaugeVec     // feed
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fondaco_requests_total",
			Help: "Pipeline requests served, by feed and response source (cache|upstream|stale|local).",
		}, []string{"feed", "source"}),
		Failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fondaco_request_failures_total",
			Help: "Pipeline requests failed, by feed and reason.",
		}, []string{"feed", "reason"}),
		UpstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fondaco_upstream_requests_total",
			Help: "Upstream fetch outcomes, by feed.",
		}, []string{"feed", "outcome"}),
		UpstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fondaco_upstream_request_duration_seconds",
			Help:    "Upstream HTTP attempt latency, by feed.",
			Buckets: prometheus.DefBuckets,
		}, []string{"feed"}),
		BreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "fondaco_upstream_breaker_state",
			Help: "Circuit breaker state per feed: 0 closed, 1 half-open, 2 open.",
		}, []string{"feed"}),
	}
	reg.MustRegister(m.Requests, m.Failures, m.UpstreamRequests, m.UpstreamDuration, m.BreakerState)
	return m
}

func (m *Metrics) request(feed string, source string) {
	if m != nil {
		m.Requests.WithLabelValues(feed, source).Inc()
	}
}

func (m *Metrics) failure(feed string, reason string) {
	if m != nil {
		m.Failures.WithLabelValues(feed, reason).Inc()
	}
}
