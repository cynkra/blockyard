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

func TestWorkersActiveGauge(t *testing.T) {
	initial := getGaugeValue(WorkersActive)
	WorkersActive.Inc()
	WorkersActive.Inc()
	after := getGaugeValue(WorkersActive)
	if after-initial != 2 {
		t.Errorf("expected +2, got %v", after-initial)
	}
	WorkersActive.Dec()
	final := getGaugeValue(WorkersActive)
	if final-initial != 1 {
		t.Errorf("expected +1, got %v", final-initial)
	}
	// Clean up
	WorkersActive.Dec()
}

func TestCounterIncrements(t *testing.T) {
	before := getCounterValue(WorkersSpawned)
	WorkersSpawned.Inc()
	after := getCounterValue(WorkersSpawned)
	if after-before != 1 {
		t.Errorf("expected +1, got %v", after-before)
	}
}

func TestHistogramObservation(t *testing.T) {
	// Just verify it doesn't panic
	ColdStartDuration.Observe(1.5)
	ProxyRequestDuration.Observe(0.05)
	BuildDuration.Observe(30.0)
}
