package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/integration"
	"github.com/cynkra/blockyard/internal/server"
)

// failingBackend wraps MockBackend but forces ListManaged to return an error.
type failingBackend struct {
	*mock.MockBackend
}

func (b *failingBackend) ListManaged(_ context.Context) ([]backend.ManagedResource, error) {
	return nil, errors.New("docker unavailable")
}

func testServerForReadyz(t *testing.T) *server.Server {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)

	cfg := &config.Config{}
	return server.NewServer(cfg, mock.New(), database)
}

// readyzReq creates a GET /readyz request with the test PAT bearer token.
func readyzReq() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	req.Header.Set("Authorization", "Bearer "+testPAT)
	return req
}

func TestReadyzAllPass(t *testing.T) {
	srv := testServerForReadyz(t)
	handler := readyzHandler(srv, false)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, readyzReq())

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

	handler := readyzHandler(srv, false)
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

func TestReadyzUnauthenticatedHidesChecks(t *testing.T) {
	srv := testServerForReadyz(t)
	handler := readyzHandler(srv, false)

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
	if _, exists := body["checks"]; exists {
		t.Error("unauthenticated response should not include checks")
	}
}

func TestMetricsEndpointEnabled(t *testing.T) {
	srv := testServerForReadyz(t)
	srv.Config.Telemetry = &config.TelemetryConfig{MetricsEnabled: true}

	router := NewRouter(srv)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+testPAT)
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

func TestReadyzWithOIDCConfigured(t *testing.T) {
	// Start a test server that returns 200 for the OIDC discovery endpoint.
	idpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"issuer":"test"}`))
	}))
	t.Cleanup(idpSrv.Close)

	srv := testServerForReadyz(t)
	srv.Config.OIDC = &config.OidcConfig{
		IssuerURL: idpSrv.URL,
	}

	handler := readyzHandler(srv, false)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, readyzReq())

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)

	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatal("expected checks map")
	}
	if checks["idp"] != "pass" {
		t.Errorf("idp = %v, want pass", checks["idp"])
	}
}

func TestReadyzWithOIDCFail(t *testing.T) {
	srv := testServerForReadyz(t)
	// Use a URL that will not resolve, causing the IDP check to fail.
	srv.Config.OIDC = &config.OidcConfig{
		IssuerURL: "http://127.0.0.1:1",
	}

	handler := readyzHandler(srv, false)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, readyzReq())

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)

	if body["status"] != "not_ready" {
		t.Errorf("expected not_ready, got %v", body["status"])
	}

	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatal("expected checks map")
	}
	if checks["idp"] != "fail" {
		t.Errorf("idp = %v, want fail", checks["idp"])
	}
}

func TestReadyzDockerFail(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	seedTestAdmin(t, database)
	cfg := &config.Config{}
	be := &failingBackend{MockBackend: mock.New()}
	srv := server.NewServer(cfg, be, database)

	handler := readyzHandler(srv, false)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, readyzReq())

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)

	if body["status"] != "not_ready" {
		t.Errorf("expected not_ready, got %v", body["status"])
	}

	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatal("expected checks map")
	}
	if checks["docker"] != "fail" {
		t.Errorf("docker = %v, want fail", checks["docker"])
	}
}

func TestReadyzWithVaultPass(t *testing.T) {
	// Mock OpenBao server that returns 200 for /v1/sys/health.
	baoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sys/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(baoSrv.Close)

	srv := testServerForReadyz(t)
	srv.VaultClient = integration.NewClient(baoSrv.URL, func() string { return "test-token" })

	handler := readyzHandler(srv, false)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, readyzReq())

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
	if checks["openbao"] != "pass" {
		t.Errorf("openbao = %v, want pass", checks["openbao"])
	}
}

func TestReadyzWithVaultFail(t *testing.T) {
	// Mock OpenBao server that returns 503 for /v1/sys/health.
	baoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(baoSrv.Close)

	srv := testServerForReadyz(t)
	srv.VaultClient = integration.NewClient(baoSrv.URL, func() string { return "test-token" })

	handler := readyzHandler(srv, false)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, readyzReq())

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)

	if body["status"] != "not_ready" {
		t.Errorf("expected not_ready, got %v", body["status"])
	}

	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatal("expected checks map")
	}
	if checks["openbao"] != "fail" {
		t.Errorf("openbao = %v, want fail", checks["openbao"])
	}
}

func TestReadyzWithSessionCookieShowsChecks(t *testing.T) {
	srv := testServerForReadyz(t)

	// Set up signing key and session store for cookie auth.
	srv.SigningKey = auth.DeriveSigningKey("test-session-secret")
	srv.UserSessions = auth.NewUserSessionStore()
	srv.UserSessions.Set("admin", &auth.UserSession{
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 3600,
	})

	cookie := &auth.CookiePayload{
		Sub:      "admin",
		IssuedAt: time.Now().Unix(),
	}
	cookieValue, err := cookie.Encode(srv.SigningKey)
	if err != nil {
		t.Fatal(err)
	}

	handler := readyzHandler(srv, false)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	req.AddCookie(&http.Cookie{Name: "blockyard_session", Value: cookieValue})
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
		t.Fatal("expected checks map in authenticated response via session cookie")
	}
	if checks["database"] != "pass" {
		t.Errorf("database = %v, want pass", checks["database"])
	}
	if checks["docker"] != "pass" {
		t.Errorf("docker = %v, want pass", checks["docker"])
	}
}

func TestReadyzWithOIDCNon200(t *testing.T) {
	// Mock IdP that returns 500 for the discovery endpoint.
	idpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(idpSrv.Close)

	srv := testServerForReadyz(t)
	srv.Config.OIDC = &config.OidcConfig{
		IssuerURL: idpSrv.URL,
	}

	handler := readyzHandler(srv, false)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, readyzReq())

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)

	if body["status"] != "not_ready" {
		t.Errorf("expected not_ready, got %v", body["status"])
	}

	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatal("expected checks map")
	}
	if checks["idp"] != "fail" {
		t.Errorf("idp = %v, want fail", checks["idp"])
	}
}

func TestReadyzTrustedAlwaysShowsChecks(t *testing.T) {
	srv := testServerForReadyz(t)
	handler := readyzHandler(srv, true)

	// Unauthenticated request — trusted handler should still show checks.
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
		t.Fatal("expected checks map in trusted readyz response")
	}
	if checks["database"] != "pass" {
		t.Errorf("database = %v, want pass", checks["database"])
	}
}

func TestManagementRouterServesEndpoints(t *testing.T) {
	srv := testServerForReadyz(t)
	srv.Config.Telemetry = &config.TelemetryConfig{MetricsEnabled: true}

	router := NewManagementRouter(srv)

	// /healthz — unauthenticated, returns "ok"
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz: expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("/healthz: expected 'ok', got %q", rec.Body.String())
	}

	// /readyz — unauthenticated, includes checks (trusted)
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/readyz: expected 200, got %d", rec.Code)
	}
	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)
	if _, ok := body["checks"]; !ok {
		t.Error("/readyz: expected checks in management listener response")
	}

	// /metrics — unauthenticated, returns prometheus data
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/metrics: expected 200, got %d", rec.Code)
	}
}

func TestManagementRouterMetricsDisabled(t *testing.T) {
	srv := testServerForReadyz(t)
	// Telemetry nil — /metrics should 404 on management router too

	router := NewManagementRouter(srv)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestMainRouterOmitsOpsWhenManagementBind(t *testing.T) {
	srv := testServerForReadyz(t)
	srv.Config.Server.ManagementBind = "127.0.0.1:9100"

	router := NewRouter(srv)

	// /healthz should 404 on the main router
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("/healthz: expected 404 on main router, got %d", rec.Code)
	}

	// /readyz should 404 on the main router
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("/readyz: expected 404 on main router, got %d", rec.Code)
	}
}
