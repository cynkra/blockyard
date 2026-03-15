package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/integration"
	"github.com/cynkra/blockyard/internal/server"
)

// mockVaultLogin creates a mock OpenBao server responding to /v1/auth/jwt/login.
func mockVaultLogin(t *testing.T, token string, leaseDuration int) *integration.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/jwt/login" && r.Method == "POST" {
			json.NewEncoder(w).Encode(map[string]any{
				"auth": map[string]any{
					"client_token":   token,
					"lease_duration": leaseDuration,
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return integration.NewClient(srv.URL, func() string { return "admin-token" })
}

// credentialServer builds a minimal server suitable for credential exchange tests.
func credentialServer(t *testing.T, vaultClient *integration.Client) *server.Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{},
		Openbao: &config.OpenbaoConfig{
			Address:     "http://mock",
			AdminToken:  config.NewSecret("admin-token"),
			TokenTTL:    config.Duration{Duration: 1 * time.Hour},
			JWTAuthPath: "jwt",
		},
	}
	return &server.Server{
		Config:          cfg,
		Workers:         server.NewWorkerMap(),
		UserSessions:    auth.NewUserSessionStore(),
		SessionTokenKey: auth.DeriveSessionTokenKey("test-secret"),
		VaultClient:     vaultClient,
		VaultTokenCache: integration.NewVaultTokenCache(),
	}
}

// issueToken creates a signed session token with the given claims.
func issueToken(t *testing.T, claims *auth.SessionTokenClaims) string {
	t.Helper()
	key := auth.DeriveSessionTokenKey("test-secret")
	tok, err := auth.EncodeSessionToken(claims, key)
	if err != nil {
		t.Fatalf("encode session token: %v", err)
	}
	return tok
}

func validClaims() *auth.SessionTokenClaims {
	now := time.Now().Unix()
	return &auth.SessionTokenClaims{
		Sub: "user-1",
		App: "app-1",
		Wid: "worker-1",
		Iat: now,
		Exp: now + int64(auth.SessionTokenTTL.Seconds()),
	}
}

func TestExchangeValidToken(t *testing.T) {
	client := mockVaultLogin(t, "s.scoped-token", 3600)
	srv := credentialServer(t, client)
	srv.Workers.Set("worker-1", server.ActiveWorker{AppID: "app-1"})
	srv.UserSessions.Set("user-1", &auth.UserSession{
		AccessToken: "oidc-access-token",
		ExpiresAt:   time.Now().Add(10 * time.Minute).Unix(),
	})

	handler := ExchangeVaultCredential(srv)
	token := issueToken(t, validClaims())

	req := httptest.NewRequest("POST", "/api/v1/credentials/vault", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["token"] != "s.scoped-token" {
		t.Errorf("token = %q, want %q", body["token"], "s.scoped-token")
	}
	if body["ttl"] != float64(3600) {
		t.Errorf("ttl = %v, want 3600", body["ttl"])
	}
}

func TestExchangeExpiredToken(t *testing.T) {
	client := mockVaultLogin(t, "s.token", 3600)
	srv := credentialServer(t, client)
	srv.Workers.Set("worker-1", server.ActiveWorker{AppID: "app-1"})

	claims := validClaims()
	claims.Iat = time.Now().Add(-10 * time.Minute).Unix()
	claims.Exp = time.Now().Add(-5 * time.Minute).Unix()
	token := issueToken(t, claims)

	req := httptest.NewRequest("POST", "/api/v1/credentials/vault", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	ExchangeVaultCredential(srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExchangeTamperedToken(t *testing.T) {
	client := mockVaultLogin(t, "s.token", 3600)
	srv := credentialServer(t, client)
	srv.Workers.Set("worker-1", server.ActiveWorker{AppID: "app-1"})

	token := issueToken(t, validClaims())
	// Flip a character in the signature portion (after the dot)
	tampered := token[:len(token)-1] + "X"

	req := httptest.NewRequest("POST", "/api/v1/credentials/vault", nil)
	req.Header.Set("Authorization", "Bearer "+tampered)
	rr := httptest.NewRecorder()

	ExchangeVaultCredential(srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExchangeUnknownWorker(t *testing.T) {
	client := mockVaultLogin(t, "s.token", 3600)
	srv := credentialServer(t, client)
	// Do not register worker-1 in the worker map.

	token := issueToken(t, validClaims())

	req := httptest.NewRequest("POST", "/api/v1/credentials/vault", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	ExchangeVaultCredential(srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExchangeAppMismatch(t *testing.T) {
	client := mockVaultLogin(t, "s.token", 3600)
	srv := credentialServer(t, client)
	// Worker belongs to a different app than what the token claims.
	srv.Workers.Set("worker-1", server.ActiveWorker{AppID: "other-app"})

	token := issueToken(t, validClaims())

	req := httptest.NewRequest("POST", "/api/v1/credentials/vault", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	ExchangeVaultCredential(srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExchangeNoVault(t *testing.T) {
	srv := credentialServer(t, nil)
	srv.VaultClient = nil
	srv.Workers.Set("worker-1", server.ActiveWorker{AppID: "app-1"})

	token := issueToken(t, validClaims())

	req := httptest.NewRequest("POST", "/api/v1/credentials/vault", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	ExchangeVaultCredential(srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExchangeNoUserSession(t *testing.T) {
	client := mockVaultLogin(t, "s.token", 3600)
	srv := credentialServer(t, client)
	srv.Workers.Set("worker-1", server.ActiveWorker{AppID: "app-1"})
	// Do not add a user session for "user-1".

	token := issueToken(t, validClaims())

	req := httptest.NewRequest("POST", "/api/v1/credentials/vault", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	ExchangeVaultCredential(srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExchangeMissingBearer(t *testing.T) {
	client := mockVaultLogin(t, "s.token", 3600)
	srv := credentialServer(t, client)

	req := httptest.NewRequest("POST", "/api/v1/credentials/vault", nil)
	// No Authorization header set.
	rr := httptest.NewRecorder()

	ExchangeVaultCredential(srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
}
