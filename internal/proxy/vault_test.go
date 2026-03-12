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
	return integration.NewClient(srv.URL, "admin-token")
}

// mockJWTLoginError creates a mock OpenBao that returns errors for JWT login.
func mockJWTLoginError(t *testing.T) *integration.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"errors":["permission denied"]}`))
	}))
	t.Cleanup(srv.Close)
	return integration.NewClient(srv.URL, "admin-token")
}

func vaultServer(t *testing.T, vaultClient *integration.Client) *server.Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{Token: config.NewSecret("test-token")},
		Openbao: &config.OpenbaoConfig{
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

func TestInjectVaultToken_SetsHeader(t *testing.T) {
	client := mockJWTLogin(t, "s.scoped-token", 3600)
	srv := vaultServer(t, client)

	r := requestWithUser("user-1", "my-access-token")
	injectVaultToken(r, srv, 1)

	got := r.Header.Get("X-Blockyard-Vault-Token")
	if got != "s.scoped-token" {
		t.Errorf("X-Blockyard-Vault-Token = %q, want %q", got, "s.scoped-token")
	}
}

func TestInjectVaultToken_StripsExistingHeader(t *testing.T) {
	// Even when VaultClient is nil, spoofed headers must be removed.
	srv := &server.Server{Config: &config.Config{}}
	r := httptest.NewRequest("GET", "/app/test-app/", nil)
	r.Header.Set("X-Blockyard-Vault-Token", "spoofed-token")

	injectVaultToken(r, srv, 1)

	if got := r.Header.Get("X-Blockyard-Vault-Token"); got != "" {
		t.Errorf("expected spoofed header to be stripped, got %q", got)
	}
}

func TestInjectVaultToken_SkipsWhenNoVaultClient(t *testing.T) {
	srv := &server.Server{Config: &config.Config{}}
	r := requestWithUser("user-1", "token")

	injectVaultToken(r, srv, 1)

	if got := r.Header.Get("X-Blockyard-Vault-Token"); got != "" {
		t.Errorf("expected no header when VaultClient is nil, got %q", got)
	}
}

func TestInjectVaultToken_SkipsWhenNoUser(t *testing.T) {
	client := mockJWTLogin(t, "s.token", 3600)
	srv := vaultServer(t, client)

	r := httptest.NewRequest("GET", "/app/test-app/", nil) // no user in context
	injectVaultToken(r, srv, 1)

	if got := r.Header.Get("X-Blockyard-Vault-Token"); got != "" {
		t.Errorf("expected no header when no user, got %q", got)
	}
}

func TestInjectVaultToken_SkipsWhenNoAccessToken(t *testing.T) {
	client := mockJWTLogin(t, "s.token", 3600)
	srv := vaultServer(t, client)

	r := requestWithUser("user-1", "") // empty access token
	injectVaultToken(r, srv, 1)

	if got := r.Header.Get("X-Blockyard-Vault-Token"); got != "" {
		t.Errorf("expected no header when access token is empty, got %q", got)
	}
}

func TestInjectVaultToken_UsesCachedToken(t *testing.T) {
	// Set up a mock that would fail if called — proving the cache is used.
	client := mockJWTLoginError(t)
	srv := vaultServer(t, client)

	// Pre-populate cache.
	srv.VaultTokenCache.Set("user-1", "s.cached-token", 1*time.Hour)

	r := requestWithUser("user-1", "my-access-token")
	injectVaultToken(r, srv, 1)

	got := r.Header.Get("X-Blockyard-Vault-Token")
	if got != "s.cached-token" {
		t.Errorf("X-Blockyard-Vault-Token = %q, want %q", got, "s.cached-token")
	}
}

func TestInjectVaultToken_CacheMissFetchesToken(t *testing.T) {
	client := mockJWTLogin(t, "s.fresh-token", 3600)
	srv := vaultServer(t, client)

	r := requestWithUser("user-1", "my-access-token")
	injectVaultToken(r, srv, 1)

	got := r.Header.Get("X-Blockyard-Vault-Token")
	if got != "s.fresh-token" {
		t.Errorf("X-Blockyard-Vault-Token = %q, want %q", got, "s.fresh-token")
	}

	// Verify it was cached.
	cached, ok := srv.VaultTokenCache.Get("user-1")
	if !ok {
		t.Fatal("expected token to be cached after fetch")
	}
	if cached != "s.fresh-token" {
		t.Errorf("cached token = %q, want %q", cached, "s.fresh-token")
	}
}

func TestInjectVaultToken_LoginErrorOmitsHeader(t *testing.T) {
	client := mockJWTLoginError(t)
	srv := vaultServer(t, client)

	r := requestWithUser("user-1", "bad-token")
	injectVaultToken(r, srv, 1)

	if got := r.Header.Get("X-Blockyard-Vault-Token"); got != "" {
		t.Errorf("expected no header on login error, got %q", got)
	}
}

func TestInjectVaultToken_SkipsForSharedContainers(t *testing.T) {
	client := mockJWTLogin(t, "s.should-not-appear", 3600)
	srv := vaultServer(t, client)

	r := requestWithUser("user-1", "my-access-token")
	injectVaultToken(r, srv, 2) // max_sessions_per_worker > 1

	if got := r.Header.Get("X-Blockyard-Vault-Token"); got != "" {
		t.Errorf("expected no vault token for shared container, got %q", got)
	}
}

func TestInjectVaultToken_ZeroTTLFallsBackToConfig(t *testing.T) {
	// Mock returns 0 lease_duration.
	client := mockJWTLogin(t, "s.zero-ttl-token", 0)
	srv := vaultServer(t, client)
	srv.Config.Openbao.TokenTTL = config.Duration{Duration: 30 * time.Minute}

	r := requestWithUser("user-1", "my-access-token")
	injectVaultToken(r, srv, 1)

	got := r.Header.Get("X-Blockyard-Vault-Token")
	if got != "s.zero-ttl-token" {
		t.Errorf("X-Blockyard-Vault-Token = %q, want %q", got, "s.zero-ttl-token")
	}

	// Token should be cached with config TTL (30m), which is above the
	// 30s renewal buffer, so it should be retrievable.
	if _, ok := srv.VaultTokenCache.Get("user-1"); !ok {
		t.Error("expected token to be cached with config TTL fallback")
	}
}
