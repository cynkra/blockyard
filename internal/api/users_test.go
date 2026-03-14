package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/integration"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/testutil"
)

// mockVaultForEnrollment creates a mock OpenBao server that accepts KV writes.
func mockVaultForEnrollment(t *testing.T) *integration.Client {
	t.Helper()
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/secret/data/") {
			lastPath = r.URL.Path
			_ = lastPath
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return integration.NewClient(srv.URL, func() string { return "admin-token" })
}

func testServerWithVault(t *testing.T, idp *testutil.MockIdP) (*server.Server, *httptest.Server) {
	t.Helper()
	tmp := t.TempDir()

	cfg := &config.Config{
		Server: config.ServerConfig{Token: config.NewSecret("test-token")},
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{MaxWorkers: 100},
		OIDC: &config.OidcConfig{
			IssuerURL:    idp.IssuerURL(),
			ClientID:     "blockyard",
			ClientSecret: config.NewSecret("test-secret"),
		},
		Openbao: &config.OpenbaoConfig{
			Address:     "http://mock-vault",
			AdminToken:  config.NewSecret("admin-token"),
			JWTAuthPath: "jwt",
		},
	}

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	be := mock.New()
	srv := server.NewServer(cfg, be, database)

	jwksCache, err := auth.NewJWKSCache(idp.IssuerURL() + "/jwks")
	if err != nil {
		t.Fatal(err)
	}
	srv.JWKSCache = jwksCache
	srv.VaultClient = mockVaultForEnrollment(t)
	srv.VaultTokenCache = integration.NewVaultTokenCache()

	handler := NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return srv, ts
}

func TestEnrollCredential_WithJWT(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	_, ts := testServerWithVault(t, idp)

	jwt := idp.IssueJWT("test-user", []string{"testers"})
	body := `{"api_key":"sk-test-key"}`
	req := jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/openai", jwt, strings.NewReader(body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}
}

func TestEnrollCredential_Unauthenticated(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	_, ts := testServerWithVault(t, idp)

	body := `{"api_key":"sk-test-key"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/credentials/openai", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestEnrollCredential_InvalidServiceName(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	_, ts := testServerWithVault(t, idp)

	jwt := idp.IssueJWT("test-user", []string{"testers"})
	body := `{"api_key":"sk-test-key"}`

	// Service name with spaces.
	req := jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/bad service", jwt, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}

	var errResp errorResponse
	json.NewDecoder(resp.Body).Decode(&errResp)
	if !strings.Contains(errResp.Message, "invalid service name") {
		t.Errorf("expected 'invalid service name' in message, got %q", errResp.Message)
	}
}

func TestEnrollCredential_MissingAPIKey(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	_, ts := testServerWithVault(t, idp)

	jwt := idp.IssueJWT("test-user", []string{"testers"})
	body := `{"api_key":""}`
	req := jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/openai", jwt, strings.NewReader(body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestEnrollCredential_NoVaultConfigured(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	// Use testServerWithOIDC (no vault).
	_, ts := testServerWithOIDC(t, idp)

	jwt := idp.IssueJWT("test-user", []string{"testers"})
	body := `{"api_key":"sk-test-key"}`
	req := jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/openai", jwt, strings.NewReader(body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

func TestEnrollCredential_WithStaticToken(t *testing.T) {
	// When OIDC is not configured, static token should work.
	_, ts := testServer(t)

	body := `{"api_key":"sk-test-key"}`
	req := authReq("POST", ts.URL+"/api/v1/users/me/credentials/openai", strings.NewReader(body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// VaultClient is nil → 503
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (no vault), got %d", resp.StatusCode)
	}
}

func TestUserAuthWithSessionCookie(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	srv, ts := testServerWithOIDC(t, idp)

	// Set up signing key and session store.
	signingKey := auth.DeriveSigningKey("test-session-secret")
	srv.SigningKey = signingKey
	srv.UserSessions = auth.NewUserSessionStore()

	// Create a session.
	srv.UserSessions.Set("cookie-user", &auth.UserSession{
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 3600,
	})

	// Encode a cookie.
	cookie := &auth.CookiePayload{
		Sub:      "cookie-user",
		IssuedAt: time.Now().Unix(),
	}
	cookieValue, err := cookie.Encode(signingKey)
	if err != nil {
		t.Fatal(err)
	}

	// The /api/v1/users/me/credentials/openai route uses UserAuth middleware.
	// VaultClient is nil so we expect 503 (not 401), proving auth succeeded.
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/credentials/openai", strings.NewReader(`{"api_key":"sk-test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "blockyard_session", Value: cookieValue})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 503 means auth passed but vault is not configured.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (auth passed, no vault), got %d", resp.StatusCode)
	}
}

func TestUserAuthWithExpiredCookie(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	srv, ts := testServerWithOIDC(t, idp)

	signingKey := auth.DeriveSigningKey("test-session-secret")
	srv.SigningKey = signingKey
	srv.UserSessions = auth.NewUserSessionStore()

	srv.UserSessions.Set("expired-user", &auth.UserSession{
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 3600,
	})

	// Create cookie with IssuedAt far in the past (2 days ago).
	cookie := &auth.CookiePayload{
		Sub:      "expired-user",
		IssuedAt: time.Now().Unix() - 2*86400,
	}
	cookieValue, err := cookie.Encode(signingKey)
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/credentials/openai", strings.NewReader(`{"api_key":"sk-test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "blockyard_session", Value: cookieValue})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for expired cookie, got %d", resp.StatusCode)
	}
}

func TestUserAuthNeitherCookieNorToken(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	_, ts := testServerWithOIDC(t, idp)

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/credentials/openai", strings.NewReader(`{"api_key":"sk-test"}`))
	req.Header.Set("Content-Type", "application/json")
	// No cookie, no Authorization header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestEnrollCredentialInvalidJSON(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	_, ts := testServerWithVault(t, idp)

	jwt := idp.IssueJWT("test-user", []string{"testers"})
	// Send invalid JSON.
	req := jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/openai", jwt, strings.NewReader(`{invalid-json`))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}

	var errResp errorResponse
	json.NewDecoder(resp.Body).Decode(&errResp)
	if !strings.Contains(errResp.Message, "invalid request body") {
		t.Errorf("expected 'invalid request body' in message, got %q", errResp.Message)
	}
}

// TestEnrollCredentialNoVault covers lines 147-150 of users.go:
// server without vault returns 503 "credential storage not configured".
func TestEnrollCredentialNoVault(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "publisher")
	token := idp.IssueJWT("user-1", []string{})

	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/test-svc", token,
			strings.NewReader(`{"api_key":"my-key"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

// TestEnrollCredentialInvalidServiceNameSpecialChars covers lines 153-156 of users.go:
// service name with special characters is rejected with 400.
func TestEnrollCredentialInvalidServiceNameSpecialChars(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	_, ts := testServerWithVault(t, idp)
	token := idp.IssueJWT("user-1", []string{"developers"})

	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/invalid!!!", token,
			strings.NewReader(`{"api_key":"my-key"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// TestEnrollCredentialBadJSON covers lines 161-163 of users.go:
// POST with bad JSON body returns 400.
func TestEnrollCredentialBadJSON(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	_, ts := testServerWithVault(t, idp)
	token := idp.IssueJWT("user-1", []string{"developers"})

	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/test-svc", token,
			strings.NewReader(`{not json`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// TestEnrollCredentialEmptyAPIKey covers lines 165-167 of users.go:
// POST with empty api_key returns 400.
func TestEnrollCredentialEmptyAPIKey(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	_, ts := testServerWithVault(t, idp)
	token := idp.IssueJWT("user-1", []string{"developers"})

	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/test-svc", token,
			strings.NewReader(`{"api_key":""}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// TestEnrollCredentialUnauthenticated covers the UserAuth middleware
// no-auth-at-all path: no cookie, no token returns 401.
func TestEnrollCredentialUnauthenticated(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	_, ts := testServerWithOIDC(t, idp)

	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/users/me/credentials/test-svc",
		strings.NewReader(`{"api_key":"my-key"}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}
