package proxy

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

// mockJWTLogin creates a mock OpenBao server that responds to JWT login requests.
func mockJWTLogin(t *testing.T, token string, leaseDuration int) *integration.Client {
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
	return integration.NewClient(srv.URL, func() string { return "admin-token" }, nil)
}

// mockJWTLoginError creates a mock OpenBao that returns errors for JWT login.
func mockJWTLoginError(t *testing.T) *integration.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"errors":["permission denied"]}`))
	}))
	t.Cleanup(srv.Close)
	return integration.NewClient(srv.URL, func() string { return "admin-token" }, nil)
}

func vaultServer(t *testing.T, vaultClient *integration.Client) *server.Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{},
		Vault: &config.VaultConfig{
			Address:     "http://mock",
			AdminToken:  config.NewSecret("admin-token"),
			TokenTTL:    config.Duration{Duration: 1 * time.Hour},
			JWTAuthPath: "jwt",
		},
	}
	srv := &server.Server{
		Config:          cfg,
		VaultClient:     vaultClient,
		VaultTokenCache: integration.NewVaultTokenCache(),
		SessionTokenKey: auth.DeriveSessionTokenKey("test-secret"),
	}
	return srv
}

func requestWithUser(sub, accessToken string) *http.Request {
	r := httptest.NewRequest("GET", "/app/test-app/", nil)
	user := &auth.AuthenticatedUser{
		Sub:         sub,
		AccessToken: accessToken,
	}
	return r.WithContext(auth.ContextWithUser(r.Context(), user))
}

func TestInjectCredentials_SingleTenant_SetsVaultToken(t *testing.T) {
	client := mockJWTLogin(t, "s.scoped-token", 3600)
	srv := vaultServer(t, client)

	r := requestWithUser("user-1", "my-access-token")
	injectCredentials(r, srv, "app-1", "worker-1", 1)

	got := r.Header.Get("X-Blockyard-Vault-Token")
	if got != "s.scoped-token" {
		t.Errorf("X-Blockyard-Vault-Token = %q, want %q", got, "s.scoped-token")
	}
	if v := r.Header.Get("X-Blockyard-Session-Token"); v != "" {
		t.Errorf("expected no session token for single-tenant, got %q", v)
	}
}

func TestInjectCredentials_StripsExistingHeaders(t *testing.T) {
	srv := &server.Server{Config: &config.Config{}}
	r := httptest.NewRequest("GET", "/app/test-app/", nil)
	r.Header.Set("X-Blockyard-Vault-Token", "spoofed-vault")
	r.Header.Set("X-Blockyard-Session-Token", "spoofed-session")
	r.Header.Set("X-Blockyard-Pg-Role", "spoofed-role")

	injectCredentials(r, srv, "app-1", "worker-1", 1)

	if got := r.Header.Get("X-Blockyard-Vault-Token"); got != "" {
		t.Errorf("expected spoofed vault header to be stripped, got %q", got)
	}
	if got := r.Header.Get("X-Blockyard-Session-Token"); got != "" {
		t.Errorf("expected spoofed session header to be stripped, got %q", got)
	}
	if got := r.Header.Get("X-Blockyard-Pg-Role"); got != "" {
		t.Errorf("expected spoofed pg-role header to be stripped, got %q", got)
	}
}

func TestInjectCredentials_NoPgRoleWhenBoardStorageDisabled(t *testing.T) {
	client := mockJWTLogin(t, "s.token", 3600)
	srv := vaultServer(t, client)
	// BoardStorage defaults to false on the Config built by vaultServer.

	r := requestWithUser("user-1", "my-access-token")
	injectCredentials(r, srv, "app-1", "worker-1", 1)

	if got := r.Header.Get("X-Blockyard-Pg-Role"); got != "" {
		t.Errorf("X-Blockyard-Pg-Role set with board_storage=false: %q", got)
	}
}

func TestInjectCredentials_SkipsWhenNoVaultClient(t *testing.T) {
	srv := &server.Server{Config: &config.Config{}}
	r := requestWithUser("user-1", "token")

	injectCredentials(r, srv, "app-1", "worker-1", 1)

	if got := r.Header.Get("X-Blockyard-Vault-Token"); got != "" {
		t.Errorf("expected no header when VaultClient is nil, got %q", got)
	}
}

func TestInjectCredentials_SkipsWhenNoUser(t *testing.T) {
	client := mockJWTLogin(t, "s.token", 3600)
	srv := vaultServer(t, client)

	r := httptest.NewRequest("GET", "/app/test-app/", nil) // no user in context
	injectCredentials(r, srv, "app-1", "worker-1", 1)

	if got := r.Header.Get("X-Blockyard-Vault-Token"); got != "" {
		t.Errorf("expected no header when no user, got %q", got)
	}
}

func TestInjectCredentials_SkipsWhenNoAccessToken(t *testing.T) {
	client := mockJWTLogin(t, "s.token", 3600)
	srv := vaultServer(t, client)

	r := requestWithUser("user-1", "") // empty access token
	injectCredentials(r, srv, "app-1", "worker-1", 1)

	if got := r.Header.Get("X-Blockyard-Vault-Token"); got != "" {
		t.Errorf("expected no header when access token is empty, got %q", got)
	}
}

func TestInjectCredentials_UsesCachedToken(t *testing.T) {
	client := mockJWTLoginError(t)
	srv := vaultServer(t, client)

	srv.VaultTokenCache.Set("user-1", "s.cached-token", 1*time.Hour)

	r := requestWithUser("user-1", "my-access-token")
	injectCredentials(r, srv, "app-1", "worker-1", 1)

	got := r.Header.Get("X-Blockyard-Vault-Token")
	if got != "s.cached-token" {
		t.Errorf("X-Blockyard-Vault-Token = %q, want %q", got, "s.cached-token")
	}
}

func TestInjectCredentials_CacheMissFetchesToken(t *testing.T) {
	client := mockJWTLogin(t, "s.fresh-token", 3600)
	srv := vaultServer(t, client)

	r := requestWithUser("user-1", "my-access-token")
	injectCredentials(r, srv, "app-1", "worker-1", 1)

	got := r.Header.Get("X-Blockyard-Vault-Token")
	if got != "s.fresh-token" {
		t.Errorf("X-Blockyard-Vault-Token = %q, want %q", got, "s.fresh-token")
	}

	cached, ok := srv.VaultTokenCache.Get("user-1")
	if !ok {
		t.Fatal("expected token to be cached after fetch")
	}
	if cached != "s.fresh-token" {
		t.Errorf("cached token = %q, want %q", cached, "s.fresh-token")
	}
}

func TestInjectCredentials_LoginErrorOmitsHeader(t *testing.T) {
	client := mockJWTLoginError(t)
	srv := vaultServer(t, client)

	r := requestWithUser("user-1", "bad-token")
	injectCredentials(r, srv, "app-1", "worker-1", 1)

	if got := r.Header.Get("X-Blockyard-Vault-Token"); got != "" {
		t.Errorf("expected no header on login error, got %q", got)
	}
}

func TestInjectCredentials_SharedContainer_InjectsSessionToken(t *testing.T) {
	client := mockJWTLogin(t, "s.should-not-appear", 3600)
	srv := vaultServer(t, client)

	r := requestWithUser("user-1", "my-access-token")
	injectCredentials(r, srv, "app-1", "worker-1", 2)

	if got := r.Header.Get("X-Blockyard-Vault-Token"); got != "" {
		t.Errorf("expected no vault token for shared container, got %q", got)
	}

	sessionToken := r.Header.Get("X-Blockyard-Session-Token")
	if sessionToken == "" {
		t.Fatal("expected X-Blockyard-Session-Token to be set for shared container")
	}

	// Decode and verify claims
	claims, err := auth.DecodeSessionToken(sessionToken, srv.SessionTokenKey)
	if err != nil {
		t.Fatalf("decode session token: %v", err)
	}
	if claims.Sub != "user-1" {
		t.Errorf("claims.Sub = %q, want %q", claims.Sub, "user-1")
	}
	if claims.App != "app-1" {
		t.Errorf("claims.App = %q, want %q", claims.App, "app-1")
	}
	if claims.Wid != "worker-1" {
		t.Errorf("claims.Wid = %q, want %q", claims.Wid, "worker-1")
	}
	if claims.Exp-claims.Iat != int64(auth.SessionTokenTTL.Seconds()) {
		t.Errorf("token TTL = %ds, want %ds", claims.Exp-claims.Iat, int64(auth.SessionTokenTTL.Seconds()))
	}
}

func TestInjectCredentials_ZeroTTLFallsBackToConfig(t *testing.T) {
	client := mockJWTLogin(t, "s.zero-ttl-token", 0)
	srv := vaultServer(t, client)
	srv.Config.Vault.TokenTTL = config.Duration{Duration: 30 * time.Minute}

	r := requestWithUser("user-1", "my-access-token")
	injectCredentials(r, srv, "app-1", "worker-1", 1)

	got := r.Header.Get("X-Blockyard-Vault-Token")
	if got != "s.zero-ttl-token" {
		t.Errorf("X-Blockyard-Vault-Token = %q, want %q", got, "s.zero-ttl-token")
	}

	if _, ok := srv.VaultTokenCache.Get("user-1"); !ok {
		t.Error("expected token to be cached with config TTL fallback")
	}
}
