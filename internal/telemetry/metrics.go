package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds every Prometheus collector used by the blockyard server.
// Create one instance per server (via [NewMetrics]) and thread it through
// the types that need to record observations. Tests should use a dedicated
// [prometheus.Registry] so parallel tests and before/after delta assertions
// stay isolated from each other.
type Metrics struct {
	// Gauges — current state
	WorkersActive  prometheus.Gauge
	SessionsActive prometheus.Gauge

	// Counters — cumulative totals
	WorkersSpawned          prometheus.Counter
	WorkersStopped          prometheus.Counter
	BundlesUploaded         prometheus.Counter
	BundleRestoresSucceeded prometheus.Counter
	BundleRestoresFailed    prometheus.Counter
	ProxyRequests           prometheus.Counter
	HealthChecksFailed      prometheus.Counter
	AuditEntriesDropped     prometheus.Counter

	// Histograms — distributions
	ColdStartDuration    prometheus.Histogram
	ProxyRequestDuration prometheus.Histogram
	BuildDuration        prometheus.Histogram
}

// NewMetrics constructs a Metrics registered with reg. Pass
// [prometheus.DefaultRegisterer] in production so the /metrics HTTP
// handler (which scrapes the default gatherer) can serve the values.
// Tests should pass [prometheus.NewRegistry] for isolation.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	auto := promauto.With(reg)
	return &Metrics{
		WorkersActive: auto.NewGauge(prometheus.GaugeOpts{
			Name: "blockyard_workers_active",
			Help: "Currently running workers",
		}),
		SessionsActive: auto.NewGauge(prometheus.GaugeOpts{
			Name: "blockyard_sessions_active",
			Help: "Active proxy sessions",
		}),

		WorkersSpawned: auto.NewCounter(prometheus.CounterOpts{
			Name: "blockyard_workers_spawned_total",
			Help: "Total workers spawned",
		}),
		WorkersStopped: auto.NewCounter(prometheus.CounterOpts{
			Name: "blockyard_workers_stopped_total",
			Help: "Total workers stopped",
		}),
		BundlesUploaded: auto.NewCounter(prometheus.CounterOpts{
			Name: "blockyard_bundles_uploaded_total",
			Help: "Total bundles uploaded",
		}),
		BundleRestoresSucceeded: auto.NewCounter(prometheus.CounterOpts{
			Name: "blockyard_bundle_restores_succeeded_total",
			Help: "Total successful bundle restores",
		}),
		BundleRestoresFailed: auto.NewCounter(prometheus.CounterOpts{
			Name: "blockyard_bundle_restores_failed_total",
			Help: "Total failed bundle restores",
		}),
		ProxyRequests: auto.NewCounter(prometheus.CounterOpts{
			Name: "blockyard_proxy_requests_total",
			Help: "Total proxied requests",
		}),
		HealthChecksFailed: auto.NewCounter(prometheus.CounterOpts{
			Name: "blockyard_health_checks_failed_total",
			Help: "Failed health checks leading to eviction",
		}),
		AuditEntriesDropped: auto.NewCounter(prometheus.CounterOpts{
			Name: "blockyard_audit_entries_dropped_total",
			Help: "Audit log entries dropped due to full buffer",
		}),

		ColdStartDuration: auto.NewHistogram(prometheus.HistogramOpts{
			Name:    "blockyard_cold_start_seconds",
			Help:    "Worker cold-start duration (spawn to healthy)",
			Buckets: prometheus.ExponentialBuckets(0.5, 2, 8), // 0.5s to 64s
		}),
		ProxyRequestDuration: auto.NewHistogram(prometheus.HistogramOpts{
			Name:    "blockyard_proxy_request_seconds",
			Help:    "Proxy request duration (excluding cold start)",
			Buckets: prometheus.DefBuckets,
		}),
		BuildDuration: auto.NewHistogram(prometheus.HistogramOpts{
			Name:    "blockyard_build_seconds",
			Help:    "Bundle restore (build) duration",
			Buckets: prometheus.ExponentialBuckets(5, 2, 8), // 5s to 640s
		}),
	}
}
