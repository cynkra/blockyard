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
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
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

	router := NewRouter(srv, func() {}, nil, context.Background())
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

	router := NewRouter(srv, func() {}, nil, context.Background())
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
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
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
	srv.VaultClient = integration.NewClient(baoSrv.URL, integration.StaticAdmin(func() string { return "test-token" }))

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
	srv.VaultClient = integration.NewClient(baoSrv.URL, integration.StaticAdmin(func() string { return "test-token" }))

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
	srv.Config.Telemetry = &config.TelemetryConfig{MetricsEnabled: true}

	router := NewRouter(srv, func() {}, nil, context.Background())

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

	// /metrics should 404 on the main router (moved to management listener)
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+testPAT)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("/metrics: expected 404 on main router, got %d", rec.Code)
	}
}

func TestMainRouterServesHealthzWithoutManagementBind(t *testing.T) {
	srv := testServerForReadyz(t)
	// ManagementBind is empty — ops endpoints should be on the main router.

	router := NewRouter(srv, func() {}, nil, context.Background())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz: expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("/healthz: expected 'ok', got %q", rec.Body.String())
	}
}

func TestReadyzResponseContentType(t *testing.T) {
	srv := testServerForReadyz(t)
	handler := readyzHandler(srv, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	handler.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestReadyzMultipleFailures(t *testing.T) {
	// Both database and docker fail simultaneously.
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	seedTestAdmin(t, database)

	cfg := &config.Config{}
	be := &failingBackend{MockBackend: mock.New()}
	srv := server.NewServer(cfg, be, database)

	// Close DB to make database check fail too.
	database.Close()

	handler := readyzHandler(srv, true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	handler.ServeHTTP(rec, req)

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
	if checks["database"] != "fail" {
		t.Errorf("database = %v, want fail", checks["database"])
	}
	if checks["docker"] != "fail" {
		t.Errorf("docker = %v, want fail", checks["docker"])
	}
}

func TestReadyzRevokedPATHidesChecks(t *testing.T) {
	srv := testServerForReadyz(t)

	// Create a second PAT and revoke it.
	revokedToken := "by_revokedtoken0000000000000000000000000000"
	hash := auth.HashPAT(revokedToken)
	_, err := srv.DB.CreatePAT("revoked-pat", hash, "admin", "revoked", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.DB.RevokePAT("revoked-pat", "admin"); err != nil {
		t.Fatal(err)
	}

	handler := readyzHandler(srv, false)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	req.Header.Set("Authorization", "Bearer "+revokedToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)
	if _, exists := body["checks"]; exists {
		t.Error("revoked PAT should not expose checks")
	}
}

func TestReadyzInactiveUserPATHidesChecks(t *testing.T) {
	srv := testServerForReadyz(t)

	// Create a second user and deactivate them.
	_, err := srv.DB.UpsertUserWithRole("inactive-user", "inactive@test", "Inactive", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	activeVal := false
	srv.DB.UpdateUser("inactive-user", db.UserUpdate{Active: &activeVal})

	inactiveToken := "by_inactivetoken000000000000000000000000000"
	hash := auth.HashPAT(inactiveToken)
	if _, err := srv.DB.CreatePAT("inactive-pat", hash, "inactive-user", "test", nil); err != nil {
		t.Fatal(err)
	}

	handler := readyzHandler(srv, false)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	req.Header.Set("Authorization", "Bearer "+inactiveToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)
	if _, exists := body["checks"]; exists {
		t.Error("inactive user PAT should not expose checks")
	}
}

func TestReadyzNonPATBearerTokenHidesChecks(t *testing.T) {
	srv := testServerForReadyz(t)
	handler := readyzHandler(srv, false)

	// Bearer token without "by_" prefix should not authenticate.
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	req.Header.Set("Authorization", "Bearer some-random-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)
	if _, exists := body["checks"]; exists {
		t.Error("non-PAT bearer token should not expose checks")
	}
}

func TestReadyzDeactivatedUserCookieHidesChecks(t *testing.T) {
	srv := testServerForReadyz(t)

	// Create a user and deactivate them.
	_, err := srv.DB.UpsertUserWithRole("deactivated", "deactivated@test", "Gone", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	activeVal := false
	srv.DB.UpdateUser("deactivated", db.UserUpdate{Active: &activeVal})

	// Set up signing key and session store.
	srv.SigningKey = auth.DeriveSigningKey("test-session-secret")
	srv.UserSessions = auth.NewUserSessionStore()
	srv.UserSessions.Set("deactivated", &auth.UserSession{
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 3600,
	})

	cookie := &auth.CookiePayload{
		Sub:      "deactivated",
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

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)
	if _, exists := body["checks"]; exists {
		t.Error("deactivated user cookie should not expose checks")
	}
}

func TestHealthzDraining(t *testing.T) {
	srv := testServerForReadyz(t)
	srv.Draining.Store(true)

	r := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	handler := NewManagementRouter(srv)
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
	if body := w.Body.String(); body != "draining" {
		t.Errorf("expected body 'draining', got %q", body)
	}
}

func TestHealthzOK(t *testing.T) {
	srv := testServerForReadyz(t)

	r := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	handler := NewManagementRouter(srv)
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if body := w.Body.String(); body != "ok" {
		t.Errorf("expected body 'ok', got %q", body)
	}
}

func TestReadyzDraining(t *testing.T) {
	srv := testServerForReadyz(t)
	srv.Draining.Store(true)

	r := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()

	handler := NewManagementRouter(srv)
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}

	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)
	if body["status"] != "draining" {
		t.Errorf("expected status 'draining', got %v", body["status"])
	}
}

func TestReadyzPassiveMode(t *testing.T) {
	srv := testServerForReadyz(t)
	srv.Passive.Store(true)

	r := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()

	handler := NewManagementRouter(srv)
	handler.ServeHTTP(w, r)

	// Should return 200 — passive servers are ready to serve.
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)
	if body["mode"] != "passive" {
		t.Errorf("expected mode 'passive', got %v", body["mode"])
	}
}

func TestReadyzPassiveModeWithFailedDeps(t *testing.T) {
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	seedTestAdmin(t, database)

	cfg := &config.Config{}
	be := &failingBackend{MockBackend: mock.New()}
	srv := server.NewServer(cfg, be, database)
	srv.Passive.Store(true)

	// Close DB so database check also fails.
	database.Close()

	handler := readyzHandler(srv, true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}

	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "not_ready" {
		t.Errorf("expected status 'not_ready', got %v", body["status"])
	}
	if body["mode"] != "passive" {
		t.Errorf("expected mode 'passive', got %v", body["mode"])
	}
}

func TestMainRouterHealthzDraining(t *testing.T) {
	srv := testServerForReadyz(t)
	srv.Draining.Store(true)
	// ManagementBind empty — /healthz is on the main router.

	router := NewRouter(srv, func() {}, nil, context.Background())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != "draining" {
		t.Errorf("expected body 'draining', got %q", body)
	}
}

func TestManagementRouterMetricsExplicitlyDisabled(t *testing.T) {
	srv := testServerForReadyz(t)
	// Telemetry is set but metrics explicitly disabled.
	srv.Config.Telemetry = &config.TelemetryConfig{MetricsEnabled: false}

	router := NewManagementRouter(srv)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}
