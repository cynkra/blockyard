package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Gauges — current state
var (
	WorkersActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "blockyard_workers_active",
		Help: "Currently running workers",
	})
	SessionsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "blockyard_sessions_active",
		Help: "Active proxy sessions",
	})
)

// Counters — cumulative totals
var (
	WorkersSpawned = promauto.NewCounter(prometheus.CounterOpts{
		Name: "blockyard_workers_spawned_total",
		Help: "Total workers spawned",
	})
	WorkersStopped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "blockyard_workers_stopped_total",
		Help: "Total workers stopped",
	})
	BundlesUploaded = promauto.NewCounter(prometheus.CounterOpts{
		Name: "blockyard_bundles_uploaded_total",
		Help: "Total bundles uploaded",
	})
	BundleRestoresSucceeded = promauto.NewCounter(prometheus.CounterOpts{
		Name: "blockyard_bundle_restores_succeeded_total",
		Help: "Total successful bundle restores",
	})
	BundleRestoresFailed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "blockyard_bundle_restores_failed_total",
		Help: "Total failed bundle restores",
	})
	ProxyRequests = promauto.NewCounter(prometheus.CounterOpts{
		Name: "blockyard_proxy_requests_total",
		Help: "Total proxied requests",
	})
	HealthChecksFailed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "blockyard_health_checks_failed_total",
		Help: "Failed health checks leading to eviction",
	})
)

// Histograms — distributions
var (
	ColdStartDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "blockyard_cold_start_seconds",
		Help:    "Worker cold-start duration (spawn to healthy)",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 8), // 0.5s to 64s
	})
	ProxyRequestDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "blockyard_proxy_request_seconds",
		Help:    "Proxy request duration (excluding cold start)",
		Buckets: prometheus.DefBuckets,
	})
	BuildDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "blockyard_build_seconds",
		Help:    "Bundle restore (build) duration",
		Buckets: prometheus.ExponentialBuckets(5, 2, 8), // 5s to 640s
	})
)
