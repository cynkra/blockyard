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
