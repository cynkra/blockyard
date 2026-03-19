package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"

	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/telemetry"
	"github.com/cynkra/blockyard/internal/testutil"
)

// --- metric snapshot helpers ---

func gaugeValue(g prometheus.Gauge) float64 {
	var m io_prometheus_client.Metric
	g.Write(&m)
	return m.GetGauge().GetValue()
}

func counterValue(c prometheus.Counter) float64 {
	var m io_prometheus_client.Metric
	c.Write(&m)
	return m.GetCounter().GetValue()
}

func histogramCount(h prometheus.Histogram) uint64 {
	var m io_prometheus_client.Metric
	h.(prometheus.Metric).Write(&m)
	return m.GetHistogram().GetSampleCount()
}

// --- tests ---

// TestMetricsProxyRequestCounter verifies that each proxy request
// increments blockyard_proxy_requests_total and records a duration
// observation in blockyard_proxy_request_seconds.
func TestMetricsProxyRequestCounter(t *testing.T) {
	_, ts := testProxyServer(t)
	createAndStartApp(t, ts, "metrics-app")

	beforeCount := counterValue(telemetry.ProxyRequests)
	beforeHist := histogramCount(telemetry.ProxyRequestDuration)

	// Two requests
	resp, err := http.Get(ts.URL + "/app/metrics-app/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/app/metrics-app/sub/path")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	afterCount := counterValue(telemetry.ProxyRequests)
	afterHist := histogramCount(telemetry.ProxyRequestDuration)

	if delta := afterCount - beforeCount; delta != 2 {
		t.Errorf("expected proxy_requests_total to increase by 2, got %v", delta)
	}
	if delta := afterHist - beforeHist; delta != 2 {
		t.Errorf("expected proxy_request_seconds observation count to increase by 2, got %v", delta)
	}
}

// TestMetricsColdStartSpawn verifies that a cold-start proxy hit
// increments workers_spawned_total, workers_active, and records a
// cold_start_seconds observation.
func TestMetricsColdStartSpawn(t *testing.T) {
	srv, ts := testProxyServer(t)

	// Create app without starting it (cold start path)
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"cold-metrics"}`)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	id := created["id"].(string)

	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	http.DefaultClient.Do(req)
	time.Sleep(200 * time.Millisecond)

	beforeSpawned := counterValue(telemetry.WorkersSpawned)
	beforeActive := gaugeValue(telemetry.WorkersActive)
	beforeColdStart := histogramCount(telemetry.ColdStartDuration)

	if srv.Workers.Count() != 0 {
		t.Fatalf("expected 0 workers before proxy hit, got %d", srv.Workers.Count())
	}

	resp, err := http.Get(ts.URL + "/app/cold-metrics/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	if delta := counterValue(telemetry.WorkersSpawned) - beforeSpawned; delta != 1 {
		t.Errorf("expected workers_spawned_total +1, got %v", delta)
	}
	if delta := gaugeValue(telemetry.WorkersActive) - beforeActive; delta != 1 {
		t.Errorf("expected workers_active +1, got %v", delta)
	}
	if delta := histogramCount(telemetry.ColdStartDuration) - beforeColdStart; delta != 1 {
		t.Errorf("expected cold_start_seconds observation count +1, got %v", delta)
	}
}

// TestMetricsSessionActive verifies that new proxy sessions increment
// sessions_active and that eviction via ops.EvictWorker decrements it
// along with incrementing workers_stopped_total.
func TestMetricsSessionActive(t *testing.T) {
	srv, ts := testProxyServer(t)
	createAndStartApp(t, ts, "sess-metrics")

	beforeSessions := gaugeValue(telemetry.SessionsActive)

	// First request creates a new session
	resp, err := http.Get(ts.URL + "/app/sess-metrics/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	afterSessions := gaugeValue(telemetry.SessionsActive)
	if delta := afterSessions - beforeSessions; delta != 1 {
		t.Errorf("expected sessions_active +1 after new session, got %v", delta)
	}

	// Second request without cookie creates another session
	_, err = http.Get(ts.URL + "/app/sess-metrics/")
	if err != nil {
		t.Fatal(err)
	}
	afterSessions2 := gaugeValue(telemetry.SessionsActive)
	if delta := afterSessions2 - beforeSessions; delta != 2 {
		t.Errorf("expected sessions_active +2 after two new sessions, got %v", delta)
	}

	// Evict the worker directly — sessions_active should drop
	beforeEvict := gaugeValue(telemetry.SessionsActive)
	beforeStopped := counterValue(telemetry.WorkersStopped)

	workerIDs := srv.Workers.All()
	if len(workerIDs) == 0 {
		t.Fatal("expected at least one worker to evict")
	}
	for _, wid := range workerIDs {
		ops.EvictWorker(context.Background(), srv, wid)
	}

	afterEvict := gaugeValue(telemetry.SessionsActive)
	if afterEvict >= beforeEvict {
		t.Errorf("expected sessions_active to decrease after eviction, before=%v after=%v",
			beforeEvict, afterEvict)
	}

	afterStopped := counterValue(telemetry.WorkersStopped)
	if delta := afterStopped - beforeStopped; delta < 1 {
		t.Errorf("expected workers_stopped_total +1 after eviction, got %v", delta)
	}
}

// TestMetricsBundleUploaded verifies that uploading a bundle increments
// blockyard_bundles_uploaded_total.
func TestMetricsBundleUploaded(t *testing.T) {
	_, ts := testProxyServer(t)

	// Create app
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"bundle-metrics"}`)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	id := created["id"].(string)

	before := counterValue(telemetry.BundlesUploaded)

	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	after := counterValue(telemetry.BundlesUploaded)
	if delta := after - before; delta != 1 {
		t.Errorf("expected bundles_uploaded_total +1, got %v", delta)
	}
}

// TestMetricsWebSocketRequest verifies that WebSocket upgrade requests
// are counted by proxy_requests_total and record a duration observation.
func TestMetricsWebSocketRequest(t *testing.T) {
	srv, ts := testProxyServer(t)
	srv.Backend.(*mock.MockBackend).SetWSHandler(wsEchoHandler())
	createAndStartApp(t, ts, "ws-metrics")

	beforeCount := counterValue(telemetry.ProxyRequests)

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/ws-metrics/"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ping" {
		t.Errorf("expected 'ping', got %q", data)
	}

	conn.Close(websocket.StatusNormalClosure, "done")

	afterCount := counterValue(telemetry.ProxyRequests)
	if delta := afterCount - beforeCount; delta < 1 {
		t.Errorf("expected proxy_requests_total to increase for WebSocket, got %v", delta)
	}
}

// TestMetricsNotFoundDoesNotRecordDuration verifies that requests for
// non-existent apps still increment proxy_requests_total (the counter
// fires at the top of the handler) but the request returns 404.
func TestMetricsNotFoundStillCounts(t *testing.T) {
	_, ts := testProxyServer(t)

	before := counterValue(telemetry.ProxyRequests)

	resp, err := http.Get(ts.URL + "/app/no-such-app/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	after := counterValue(telemetry.ProxyRequests)
	if delta := after - before; delta != 1 {
		t.Errorf("expected proxy_requests_total +1 even for 404, got %v", delta)
	}
}

// TestMetricsCapacityDoesNotSpawn verifies that hitting the worker
// capacity limit does not increment workers_spawned_total.
func TestMetricsCapacityDoesNotSpawn(t *testing.T) {
	srv, ts := testProxyServer(t)

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"cap-metrics"}`)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	id := created["id"].(string)

	srv.DB.CreateBundle("b-cap", id)
	srv.DB.UpdateBundleStatus("b-cap", "ready")
	srv.DB.SetActiveBundle(id, "b-cap")

	// Fill to capacity
	for i := range srv.Config.Proxy.MaxWorkers {
		srv.Workers.Set(
			fmt.Sprintf("cap-fake-%d", i),
			server.ActiveWorker{AppID: "fake"},
		)
	}

	beforeSpawned := counterValue(telemetry.WorkersSpawned)

	resp, err := http.Get(ts.URL + "/app/cap-metrics/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	afterSpawned := counterValue(telemetry.WorkersSpawned)
	if delta := afterSpawned - beforeSpawned; delta != 0 {
		t.Errorf("expected no change to workers_spawned_total at capacity, got %v", delta)
	}
}

