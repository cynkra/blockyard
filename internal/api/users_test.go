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
		if r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/v1/secret/data/") {
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
		Server: config.ServerConfig{},
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

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	be := mock.New()
	srv := server.NewServer(cfg, be, database)

	srv.VaultClient = mockVaultForEnrollment(t)
	srv.VaultTokenCache = integration.NewVaultTokenCache()

	handler := NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return srv, ts
}

func TestEnrollCredential_WithPAT(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	srv, ts := testServerWithVault(t, idp)

	srv.DB.UpsertUserWithRole("test-user", "test@example.com", "Test User", "viewer")
	pat := createTestPAT(t, srv.DB, "test-user")
	body := `{"api_key":"sk-test-key"}`
	req := jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/openai", pat, strings.NewReader(body))

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

	srv, ts := testServerWithVault(t, idp)

	srv.DB.UpsertUserWithRole("test-user", "test@example.com", "Test User", "viewer")
	pat := createTestPAT(t, srv.DB, "test-user")
	body := `{"api_key":"sk-test-key"}`

	// Service name with spaces.
	req := jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/bad service", pat, strings.NewReader(body))
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

	srv, ts := testServerWithVault(t, idp)

	srv.DB.UpsertUserWithRole("test-user", "test@example.com", "Test User", "viewer")
	pat := createTestPAT(t, srv.DB, "test-user")
	body := `{"api_key":""}`
	req := jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/openai", pat, strings.NewReader(body))

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
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("test-user", "test@example.com", "Test User", "viewer")
	pat := createTestPAT(t, srv.DB, "test-user")
	body := `{"api_key":"sk-test-key"}`
	req := jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/openai", pat, strings.NewReader(body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

// sessionCookie creates a valid session cookie for the given user sub,
// setting up the signing key and session store on the server if needed.
func sessionCookie(t *testing.T, srv *server.Server, sub string) *http.Cookie {
	t.Helper()
	if srv.SigningKey == nil {
		srv.SigningKey = auth.DeriveSigningKey("test-session-secret")
	}
	if srv.UserSessions == nil {
		srv.UserSessions = auth.NewUserSessionStore()
	}
	srv.UserSessions.Set(sub, &auth.UserSession{
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 3600,
	})
	cookie := &auth.CookiePayload{
		Sub:      sub,
		IssuedAt: time.Now().Unix(),
	}
	val, err := cookie.Encode(srv.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "blockyard_session", Value: val}
}

// --- User management endpoint tests (admin only) ---

func TestListUsers(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "admin-1")

	resp, err := http.DefaultClient.Do(jwtReq("GET", ts.URL+"/api/v1/users", pat, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var users []db.UserRow
	json.NewDecoder(resp.Body).Decode(&users)
	if len(users) != 2 {
		t.Errorf("expected 2 users, got %d", len(users))
	}
}

func TestListUsers_Forbidden(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "user-1")

	resp, err := http.DefaultClient.Do(jwtReq("GET", ts.URL+"/api/v1/users", pat, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestGetUser(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "admin-1")

	resp, err := http.DefaultClient.Do(jwtReq("GET", ts.URL+"/api/v1/users/user-1", pat, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var user db.UserRow
	json.NewDecoder(resp.Body).Decode(&user)
	if user.Sub != "user-1" {
		t.Errorf("expected sub 'user-1', got %q", user.Sub)
	}
}

func TestGetUser_NotFound(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	pat := createTestPAT(t, srv.DB, "admin-1")

	resp, err := http.DefaultClient.Do(jwtReq("GET", ts.URL+"/api/v1/users/nonexistent", pat, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestGetUser_Forbidden(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "user-1")

	resp, err := http.DefaultClient.Do(jwtReq("GET", ts.URL+"/api/v1/users/user-1", pat, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestUpdateUser_ChangeRole(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "admin-1")

	body := `{"role":"publisher"}`
	resp, err := http.DefaultClient.Do(jwtReq("PATCH", ts.URL+"/api/v1/users/user-1", pat, strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var user db.UserRow
	json.NewDecoder(resp.Body).Decode(&user)
	if user.Role != "publisher" {
		t.Errorf("expected role 'publisher', got %q", user.Role)
	}
}

func TestUpdateUser_Deactivate(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "admin-1")

	body := `{"active":false}`
	resp, err := http.DefaultClient.Do(jwtReq("PATCH", ts.URL+"/api/v1/users/user-1", pat, strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var user db.UserRow
	json.NewDecoder(resp.Body).Decode(&user)
	if user.Active {
		t.Error("expected user to be inactive")
	}
}

func TestUpdateUser_SelfModification(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	pat := createTestPAT(t, srv.DB, "admin-1")

	body := `{"role":"viewer"}`
	resp, err := http.DefaultClient.Do(jwtReq("PATCH", ts.URL+"/api/v1/users/admin-1", pat, strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestUpdateUser_InvalidRole(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "admin-1")

	body := `{"role":"superuser"}`
	resp, err := http.DefaultClient.Do(jwtReq("PATCH", ts.URL+"/api/v1/users/user-1", pat, strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestUpdateUser_NothingToUpdate(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "admin-1")

	body := `{}`
	resp, err := http.DefaultClient.Do(jwtReq("PATCH", ts.URL+"/api/v1/users/user-1", pat, strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestUpdateUser_NotFound(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	pat := createTestPAT(t, srv.DB, "admin-1")

	body := `{"role":"viewer"}`
	resp, err := http.DefaultClient.Do(jwtReq("PATCH", ts.URL+"/api/v1/users/nonexistent", pat, strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestUpdateUser_Forbidden(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "user-1")

	body := `{"role":"admin"}`
	resp, err := http.DefaultClient.Do(jwtReq("PATCH", ts.URL+"/api/v1/users/user-1", pat, strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestUpdateUser_InvalidJSON(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "admin-1")

	resp, err := http.DefaultClient.Do(jwtReq("PATCH", ts.URL+"/api/v1/users/user-1", pat, strings.NewReader(`{bad`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// --- PAT lifecycle endpoint tests ---

func TestCreateToken(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	cookie := sessionCookie(t, srv, "user-1")

	body := `{"name":"my-token","expires_in":"90d"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var result createTokenResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Name != "my-token" {
		t.Errorf("expected name 'my-token', got %q", result.Name)
	}
	if !strings.HasPrefix(result.Token, "by_") {
		t.Errorf("expected token to start with 'by_', got %q", result.Token)
	}
	if result.ExpiresAt == nil {
		t.Error("expected expires_at to be set")
	}
}

func TestCreateToken_NoExpiry(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	cookie := sessionCookie(t, srv, "user-1")

	body := `{"name":"permanent-token"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var result createTokenResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if result.ExpiresAt != nil {
		t.Error("expected expires_at to be nil for token without expiry")
	}
}

func TestCreateToken_ForbiddenViaPAT(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "user-1")

	body := `{"name":"sneaky-token"}`
	resp, err := http.DefaultClient.Do(jwtReq("POST", ts.URL+"/api/v1/users/me/tokens", pat, strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 (PATs cannot create PATs), got %d", resp.StatusCode)
	}
}

func TestCreateToken_MissingName(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	cookie := sessionCookie(t, srv, "user-1")

	body := `{"name":""}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCreateToken_InvalidExpiry(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	cookie := sessionCookie(t, srv, "user-1")

	body := `{"name":"tok","expires_in":"forever"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCreateToken_InvalidJSON(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	cookie := sessionCookie(t, srv, "user-1")

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/tokens", strings.NewReader(`{bad`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestListTokens(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	createTestPAT(t, srv.DB, "user-1")
	createTestPAT(t, srv.DB, "user-1")
	pat := createTestPAT(t, srv.DB, "user-1")

	resp, err := http.DefaultClient.Do(jwtReq("GET", ts.URL+"/api/v1/users/me/tokens", pat, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var pats []db.PATRow
	json.NewDecoder(resp.Body).Decode(&pats)
	if len(pats) != 3 {
		t.Errorf("expected 3 tokens, got %d", len(pats))
	}
}

func TestListTokens_Empty(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "user-1")

	resp, err := http.DefaultClient.Do(jwtReq("GET", ts.URL+"/api/v1/users/me/tokens", pat, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var pats []db.PATRow
	json.NewDecoder(resp.Body).Decode(&pats)
	// At least the PAT used for auth itself exists.
	if len(pats) < 1 {
		t.Errorf("expected at least 1 token, got %d", len(pats))
	}
}

func TestListTokens_Unauthenticated(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	_, ts := testServerWithOIDC(t, idp)

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/me/tokens", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestRevokeToken(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	cookie := sessionCookie(t, srv, "user-1")

	// Create a token via the API to get its ID.
	body := `{"name":"to-revoke"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created createTokenResponse
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// Revoke it.
	req, _ = http.NewRequest("DELETE", ts.URL+"/api/v1/users/me/tokens/"+created.ID, nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}
}

func TestRevokeToken_NotFound(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "user-1")

	resp, err := http.DefaultClient.Do(jwtReq("DELETE", ts.URL+"/api/v1/users/me/tokens/nonexistent-id", pat, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRevokeAllTokens(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	cookie := sessionCookie(t, srv, "user-1")

	// Create a couple of tokens.
	for _, name := range []string{"tok-1", "tok-2"} {
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/tokens",
			strings.NewReader(`{"name":"`+name+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// Revoke all.
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/v1/users/me/tokens", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}
}

func TestRevokeAllTokens_Unauthenticated(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	_, ts := testServerWithOIDC(t, idp)

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/v1/users/me/tokens", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// --- parseDuration unit tests ---

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
		ok    bool
	}{
		{"90d", 90 * 24 * time.Hour, true},
		{"24h", 24 * time.Hour, true},
		{"30m", 30 * time.Minute, true},
		{"1d", 24 * time.Hour, true},
		{"", 0, false},
		{"forever", 0, false},
		{"10s", 0, false},   // seconds not supported
		{"abc", 0, false},
		{"10", 0, false},    // missing unit
		{"-5d", 0, false},   // negative
	}

	for _, tt := range tests {
		got, ok := parseDuration(tt.input)
		if ok != tt.ok {
			t.Errorf("parseDuration(%q): ok = %v, want %v", tt.input, ok, tt.ok)
		}
		if got != tt.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tt.input, got, tt.want)
		}
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

	srv, ts := testServerWithVault(t, idp)

	srv.DB.UpsertUserWithRole("test-user", "test@example.com", "Test User", "viewer")
	pat := createTestPAT(t, srv.DB, "test-user")
	// Send invalid JSON.
	req := jwtReq("POST", ts.URL+"/api/v1/users/me/credentials/openai", pat, strings.NewReader(`{invalid-json`))

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

func boolPtr(b bool) *bool { return &b }

// --- authenticateFromPAT edge case tests ---

func TestRevokedPATRejected(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "user-1")

	// Revoke the PAT. The ID is plaintext[3:9] per createTestPAT.
	patID := pat[3:9]
	srv.DB.RevokePAT(patID, "user-1")

	resp, err := http.DefaultClient.Do(jwtReq("GET", ts.URL+"/api/v1/apps", pat, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for revoked PAT, got %d", resp.StatusCode)
	}
}

func TestExpiredPATRejected(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")

	// Create a PAT with an expiry in the past.
	plaintext, hash, err := auth.GeneratePAT()
	if err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := srv.DB.CreatePAT(plaintext[3:9], hash, "user-1", "expired-pat", &expired); err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(jwtReq("GET", ts.URL+"/api/v1/apps", plaintext, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for expired PAT, got %d", resp.StatusCode)
	}
}

func TestInactiveUserPATRejected(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	pat := createTestPAT(t, srv.DB, "user-1")

	// Deactivate the user.
	srv.DB.UpdateUser("user-1", db.UserUpdate{Active: boolPtr(false)})

	resp, err := http.DefaultClient.Do(jwtReq("GET", ts.URL+"/api/v1/apps", pat, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for inactive user PAT, got %d", resp.StatusCode)
	}
}

func TestNonByPrefixBearerTokenRejected(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	_, ts := testServerWithOIDC(t, idp)

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer some-random-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for non-by_ bearer token, got %d", resp.StatusCode)
	}
}

// TestEnrollCredentialNoVault covers lines 147-150 of users.go:
// server without vault returns 503 "credential storage not configured".
func TestEnrollCredentialNoVault(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "publisher")
	token := createTestPAT(t, srv.DB, "user-1")

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

	srv, ts := testServerWithVault(t, idp)
	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	token := createTestPAT(t, srv.DB, "user-1")

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

	srv, ts := testServerWithVault(t, idp)
	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	token := createTestPAT(t, srv.DB, "user-1")

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

	srv, ts := testServerWithVault(t, idp)
	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "viewer")
	token := createTestPAT(t, srv.DB, "user-1")

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
