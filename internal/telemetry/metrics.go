package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Reason labels for blockyard_workers_stopped_total. Keep the set small
// and bounded — cardinality is #reasons, shared across all apps.
const (
	ReasonGraceful    = "graceful"     // shutdown, app stop/disable, drain completed
	ReasonCrashed     = "crashed"      // health check failed or backend unreachable
	ReasonIdleTimeout = "idle_timeout" // autoscaler evicted an idle worker
)

// State labels for blockyard_workers gauge. Bounded and shared across
// all apps. States are derived from ActiveWorker fields on each
// reconciliation tick rather than tracked as explicit transitions, so
// "starting" and "crashed" (both transient) are not represented —
// workers only appear in the gauge once WorkersActive sees them, and
// disappear the moment they're evicted.
const (
	StateBusy     = "busy"     // has one or more active sessions
	StateIdle     = "idle"     // session count is zero, not draining
	StateDraining = "draining" // marked for drain; no new sessions routed
)

// AppUnknown is the label value used for proxy requests that do not
// resolve to an app (e.g. 404 on an unknown name). Using a sentinel
// keeps cardinality bounded against arbitrary URL paths.
const AppUnknown = "unknown"

// ProxyStatusClass returns the "Nxx" status class for an HTTP status
// code. Bucketing to classes (rather than raw codes) keeps label
// cardinality bounded — upstream apps and proxies emit long-tail codes
// (418, 499, 520, …) that would otherwise create a new series each.
func ProxyStatusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}

// Metrics holds every Prometheus collector used by the blockyard server.
// Create one instance per server (via [NewMetrics]) and thread it through
// the types that need to record observations. Tests should use a dedicated
// [prometheus.Registry] so parallel tests and before/after delta assertions
// stay isolated from each other.
type Metrics struct {
	// Gauges — current state
	WorkersActive  prometheus.Gauge
	WorkersByState *prometheus.GaugeVec // labels: state
	SessionsActive prometheus.Gauge

	// Counters — cumulative totals
	WorkersSpawned          prometheus.Counter
	WorkersStopped          *prometheus.CounterVec // labels: reason
	BundlesUploaded         prometheus.Counter
	BundleRestoresSucceeded prometheus.Counter
	BundleRestoresFailed    prometheus.Counter
	ProxyRequests           *prometheus.CounterVec // labels: app, status
	HealthChecksFailed      *prometheus.CounterVec // labels: app
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
		WorkersByState: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "blockyard_workers",
			Help: "Currently running workers, by state (busy/idle/draining)",
		}, []string{"state"}),
		SessionsActive: auto.NewGauge(prometheus.GaugeOpts{
			Name: "blockyard_sessions_active",
			Help: "Active proxy sessions",
		}),

		WorkersSpawned: auto.NewCounter(prometheus.CounterOpts{
			Name: "blockyard_workers_spawned_total",
			Help: "Total workers spawned",
		}),
		WorkersStopped: auto.NewCounterVec(prometheus.CounterOpts{
			Name: "blockyard_workers_stopped_total",
			Help: "Total workers stopped, by reason (graceful/crashed/idle_timeout)",
		}, []string{"reason"}),
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
		ProxyRequests: auto.NewCounterVec(prometheus.CounterOpts{
			Name: "blockyard_proxy_requests_total",
			Help: "Total proxied requests, by app and HTTP status class (2xx/3xx/4xx/5xx). app=\"unknown\" for requests that do not resolve to an app",
		}, []string{"app", "status"}),
		HealthChecksFailed: auto.NewCounterVec(prometheus.CounterOpts{
			Name: "blockyard_health_checks_failed_total",
			Help: "Failed health checks leading to eviction, by app",
		}, []string{"app"}),
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
