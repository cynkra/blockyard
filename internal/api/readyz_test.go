package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

func testServerForReadyz(t *testing.T) *server.Server {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := &config.Config{
		Server: config.ServerConfig{
			Token: config.NewSecret("test-token"),
		},
	}
	return server.NewServer(cfg, mock.New(), database)
}

func TestReadyzAllPass(t *testing.T) {
	srv := testServerForReadyz(t)
	handler := readyzHandler(srv)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)

	if body["status"] != "ready" {
		t.Errorf("expected ready, got %v", body["status"])
	}

	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatal("expected checks map")
	}
	if checks["database"] != "pass" {
		t.Errorf("database = %v, want pass", checks["database"])
	}
	if checks["docker"] != "pass" {
		t.Errorf("docker = %v, want pass", checks["docker"])
	}
	// IdP and OpenBao should not be present when not configured
	if _, exists := checks["idp"]; exists {
		t.Error("idp check should not be present when OIDC is not configured")
	}
	if _, exists := checks["openbao"]; exists {
		t.Error("openbao check should not be present when VaultClient is nil")
	}
}

func TestReadyzDatabaseFail(t *testing.T) {
	srv := testServerForReadyz(t)
	// Close DB to cause a failure
	srv.DB.Close()

	handler := readyzHandler(srv)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "not_ready" {
		t.Errorf("expected not_ready, got %v", body["status"])
	}
}

func TestMetricsEndpointEnabled(t *testing.T) {
	srv := testServerForReadyz(t)
	srv.Config.Telemetry = &config.TelemetryConfig{MetricsEnabled: true}

	router := NewRouter(srv)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Error("expected Content-Type header")
	}
}

func TestMetricsEndpointDisabled(t *testing.T) {
	srv := testServerForReadyz(t)
	// Telemetry nil — /metrics should 404

	router := NewRouter(srv)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}
