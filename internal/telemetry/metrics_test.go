package telemetry

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
)

func getGaugeValue(g prometheus.Gauge) float64 {
	var m io_prometheus_client.Metric
	g.Write(&m)
	return m.GetGauge().GetValue()
}

func getCounterValue(c prometheus.Counter) float64 {
	var m io_prometheus_client.Metric
	c.Write(&m)
	return m.GetCounter().GetValue()
}

func newTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	return NewMetrics(prometheus.NewRegistry())
}

func TestWorkersActiveGauge(t *testing.T) {
	m := newTestMetrics(t)
	m.WorkersActive.Inc()
	m.WorkersActive.Inc()
	if got := getGaugeValue(m.WorkersActive); got != 2 {
		t.Errorf("expected 2, got %v", got)
	}
	m.WorkersActive.Dec()
	if got := getGaugeValue(m.WorkersActive); got != 1 {
		t.Errorf("expected 1, got %v", got)
	}
}

func TestCounterIncrements(t *testing.T) {
	m := newTestMetrics(t)
	m.WorkersSpawned.Inc()
	if got := getCounterValue(m.WorkersSpawned); got != 1 {
		t.Errorf("expected 1, got %v", got)
	}
}

func TestHistogramObservation(t *testing.T) {
	m := newTestMetrics(t)
	// Just verify it doesn't panic
	m.ColdStartDuration.Observe(1.5)
	m.ProxyRequestDuration.Observe(0.05)
	m.BuildDuration.Observe(30.0)
}

func TestNewMetricsIsolation(t *testing.T) {
	// Two Metrics instances with different registries must not share state.
	a := NewMetrics(prometheus.NewRegistry())
	b := NewMetrics(prometheus.NewRegistry())

	a.WorkersSpawned.Inc()
	a.WorkersSpawned.Inc()

	if got := getCounterValue(a.WorkersSpawned); got != 2 {
		t.Errorf("a.WorkersSpawned: expected 2, got %v", got)
	}
	if got := getCounterValue(b.WorkersSpawned); got != 0 {
		t.Errorf("b.WorkersSpawned: expected 0 (isolated), got %v", got)
	}
}

func TestProxyStatusClass(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{100, "1xx"},
		{101, "1xx"},
		{200, "2xx"},
		{299, "2xx"},
		{301, "3xx"},
		{404, "4xx"},
		{418, "4xx"},
		{499, "4xx"},
		{500, "5xx"},
		{503, "5xx"},
		{520, "5xx"}, // long-tail upstream code (Cloudflare)
	}
	for _, c := range cases {
		if got := ProxyStatusClass(c.code); got != c.want {
			t.Errorf("ProxyStatusClass(%d) = %q, want %q", c.code, got, c.want)
		}
	}
}

func TestLabeledMetricsDoNotPanic(t *testing.T) {
	// Smoke-test the *Vec collectors so a wrong label arity is
	// caught here rather than at the first scrape in production.
	m := newTestMetrics(t)
	m.ProxyRequests.WithLabelValues("app1", "2xx").Inc()
	m.ProxyRequests.WithLabelValues("app1", "5xx").Inc()
	m.HealthChecksFailed.WithLabelValues("app1").Inc()
	m.WorkersStopped.WithLabelValues(ReasonGraceful).Inc()
	m.WorkersStopped.WithLabelValues(ReasonCrashed).Inc()
	m.WorkersByState.WithLabelValues(StateBusy).Set(3)
	m.WorkersByState.WithLabelValues(StateIdle).Set(7)
	m.WorkersByState.WithLabelValues(StateDraining).Set(1)
}
