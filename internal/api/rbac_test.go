package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/testutil"
)

// testServerWithOIDC creates a test server with OIDC configured.
func testServerWithOIDC(t *testing.T, idp *testutil.MockIdP) (*server.Server, *httptest.Server) {
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
	}

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	be := mock.New()
	srv := server.NewServer(cfg, be, database)

	handler := NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return srv, ts
}

// createTestPAT creates a PAT for the given user and returns the plaintext bearer token.
func createTestPAT(t *testing.T, database *db.DB, sub string) string {
	t.Helper()
	plaintext, hash, err := auth.GeneratePAT()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreatePAT(plaintext[3:9], hash, sub, "test", nil); err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func jwtReq(method, url, token string, body io.Reader) *http.Request {
	req, _ := http.NewRequest(method, url, body)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestPublisherCanCreateApp(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User 1", "publisher")
	token := createTestPAT(t, srv.DB, "user-1")

	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", token,
			strings.NewReader(`{"name":"my-app"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}

	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["owner"] != "user-1" {
		t.Errorf("expected owner 'user-1', got %v", body["owner"])
	}
}

func TestViewerCannotCreateApp(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("user-2", "user2@example.com", "User 2", "viewer")
	token := createTestPAT(t, srv.DB, "user-2")

	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", token,
			strings.NewReader(`{"name":"my-app"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestAdminSeesAllApps(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	srv.DB.UpsertUserWithRole("publisher-1", "pub1@example.com", "Publisher 1", "publisher")

	// Publisher creates an app
	pubToken := createTestPAT(t, srv.DB, "publisher-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", pubToken,
			strings.NewReader(`{"name":"app-1"}`)))
	resp.Body.Close()

	// Admin lists all apps
	adminToken := createTestPAT(t, srv.DB, "admin-1")
	resp, err := http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps", adminToken, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var apps []map[string]any
	json.NewDecoder(resp.Body).Decode(&apps)
	if len(apps) != 1 {
		t.Errorf("expected 1 app, got %d", len(apps))
	}
}

func TestPublisherSeesOnlyOwnAndGrantedApps(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("publisher-1", "pub1@example.com", "Publisher 1", "publisher")
	srv.DB.UpsertUserWithRole("publisher-2", "pub2@example.com", "Publisher 2", "publisher")

	// Publisher-1 creates app-1
	token1 := createTestPAT(t, srv.DB, "publisher-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", token1,
			strings.NewReader(`{"name":"app-1"}`)))
	var app1 map[string]any
	json.NewDecoder(resp.Body).Decode(&app1)
	resp.Body.Close()

	// Publisher-2 creates app-2
	token2 := createTestPAT(t, srv.DB, "publisher-2")
	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", token2,
			strings.NewReader(`{"name":"app-2"}`)))
	resp.Body.Close()

	// Publisher-1 should only see app-1 (not app-2)
	resp, _ = http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps", token1, nil))
	var apps []map[string]any
	json.NewDecoder(resp.Body).Decode(&apps)
	resp.Body.Close()

	if len(apps) != 1 {
		t.Fatalf("expected 1 app, got %d", len(apps))
	}
	if apps[0]["name"] != "app-1" {
		t.Errorf("expected app-1, got %v", apps[0]["name"])
	}

	// Grant viewer access to publisher-1 on app-2
	app2ID := ""
	resp, _ = http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps", token2, nil))
	var pub2Apps []map[string]any
	json.NewDecoder(resp.Body).Decode(&pub2Apps)
	resp.Body.Close()
	for _, a := range pub2Apps {
		if a["name"] == "app-2" {
			app2ID = a["id"].(string)
		}
	}

	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+app2ID+"/access", token2,
			strings.NewReader(`{"principal":"publisher-1","kind":"user","role":"viewer"}`)))
	resp.Body.Close()

	// Now publisher-1 should see both
	resp, _ = http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps", token1, nil))
	json.NewDecoder(resp.Body).Decode(&apps)
	resp.Body.Close()

	if len(apps) != 2 {
		t.Errorf("expected 2 apps after grant, got %d", len(apps))
	}
}

func TestDeleteAppRequiresOwnerOrAdmin(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner-1", "owner@example.com", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("collab-1", "collab@example.com", "Collab", "publisher")

	// Publisher creates app
	ownerToken := createTestPAT(t, srv.DB, "owner-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant collaborator access to another user
	srv.DB.GrantAppAccess(appID, "collab-1", "user", "collaborator", "owner-1")

	// Collaborator cannot delete (gets 404)
	collabToken := createTestPAT(t, srv.DB, "collab-1")
	resp, _ = http.DefaultClient.Do(
		jwtReq("DELETE", ts.URL+"/api/v1/apps/"+appID, collabToken, nil))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("collaborator delete: expected 404, got %d", resp.StatusCode)
	}

	// Owner can delete
	resp, _ = http.DefaultClient.Do(
		jwtReq("DELETE", ts.URL+"/api/v1/apps/"+appID, ownerToken, nil))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("owner delete: expected 204, got %d", resp.StatusCode)
	}
}

func TestUnmappedUserHasNoRole(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	// User with viewer role has no create/write permissions
	srv.DB.UpsertUserWithRole("unmapped-user", "unmapped@example.com", "Unmapped", "viewer")
	token := createTestPAT(t, srv.DB, "unmapped-user")

	// Cannot create apps (403)
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", token,
			strings.NewReader(`{"name":"my-app"}`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}

	// Empty list
	resp, _ = http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps", token, nil))
	var apps []map[string]any
	json.NewDecoder(resp.Body).Decode(&apps)
	resp.Body.Close()
	if len(apps) != 0 {
		t.Errorf("expected 0 apps, got %d", len(apps))
	}
}

func TestACLGrantRevokeCycle(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	adminToken := createTestPAT(t, srv.DB, "admin-1")

	// Admin creates app
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", adminToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant viewer access to user-2
	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/access", adminToken,
			strings.NewReader(`{"principal":"user-2","kind":"user","role":"viewer"}`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("grant: expected 204, got %d", resp.StatusCode)
	}

	// List grants
	resp, _ = http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps/"+appID+"/access", adminToken, nil))
	var grants []map[string]any
	json.NewDecoder(resp.Body).Decode(&grants)
	resp.Body.Close()
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(grants))
	}
	if grants[0]["principal"] != "user-2" {
		t.Errorf("expected principal user-2, got %v", grants[0]["principal"])
	}

	// Revoke access
	resp, _ = http.DefaultClient.Do(
		jwtReq("DELETE", ts.URL+"/api/v1/apps/"+appID+"/access/user/user-2", adminToken, nil))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: expected 204, got %d", resp.StatusCode)
	}

	// Verify grants are empty
	resp, _ = http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps/"+appID+"/access", adminToken, nil))
	json.NewDecoder(resp.Body).Decode(&grants)
	resp.Body.Close()
	if len(grants) != 0 {
		t.Errorf("expected 0 grants after revoke, got %d", len(grants))
	}
}

func TestStaticTokenFallback(t *testing.T) {
	// No OIDC config — v0 compat mode. Static token should still work
	// and give admin identity.
	_, ts := testServer(t)

	// Create app with static token
	resp, _ := http.DefaultClient.Do(
		authReq("POST", ts.URL+"/api/v1/apps",
			strings.NewReader(`{"name":"my-app"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	if app["owner"] != "admin" {
		t.Errorf("expected owner 'admin', got %v", app["owner"])
	}
}

func TestSetAccessType(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner-1", "owner@example.com", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("collab-1", "collab@example.com", "Collab", "publisher")
	ownerToken := createTestPAT(t, srv.DB, "owner-1")

	// Create app
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Default access_type is "acl"
	if app["access_type"] != "acl" {
		t.Errorf("default access_type = %v, want 'acl'", app["access_type"])
	}

	// Owner sets access_type to "public"
	resp, _ = http.DefaultClient.Do(
		jwtReq("PATCH", ts.URL+"/api/v1/apps/"+appID, ownerToken,
			strings.NewReader(`{"access_type":"public"}`)))
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set access_type: expected 200, got %d", resp.StatusCode)
	}
	if app["access_type"] != "public" {
		t.Errorf("access_type = %v, want 'public'", app["access_type"])
	}

	// Collaborator cannot change access_type
	srv.DB.GrantAppAccess(appID, "collab-1", "user", "collaborator", "owner-1")
	collabToken := createTestPAT(t, srv.DB, "collab-1")
	resp, _ = http.DefaultClient.Do(
		jwtReq("PATCH", ts.URL+"/api/v1/apps/"+appID, collabToken,
			strings.NewReader(`{"access_type":"acl"}`)))
	resp.Body.Close()
	// Returns 404 (hides existence from insufficient-permission callers)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("collaborator set access_type: expected 404, got %d", resp.StatusCode)
	}

	// Invalid access_type rejected (by owner)
	resp, _ = http.DefaultClient.Do(
		jwtReq("PATCH", ts.URL+"/api/v1/apps/"+appID, ownerToken,
			strings.NewReader(`{"access_type":"invalid"}`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid access_type: expected 400, got %d", resp.StatusCode)
	}
}

func TestContentViewerCannotDeploy(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner-1", "owner@example.com", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("viewer-1", "viewer@example.com", "Viewer", "publisher")

	// Publisher creates app
	ownerToken := createTestPAT(t, srv.DB, "owner-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant viewer access
	srv.DB.GrantAppAccess(appID, "viewer-1", "user", "viewer", "owner-1")

	// Viewer attempts deploy -> 404
	viewerToken := createTestPAT(t, srv.DB, "viewer-1")
	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/bundles", viewerToken,
			strings.NewReader("fake-bundle")))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("viewer deploy: expected 404, got %d", resp.StatusCode)
	}
}

func TestSelfGrantRejected(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	adminToken := createTestPAT(t, srv.DB, "admin-1")

	// Admin creates app
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", adminToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Cannot self-grant
	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/access", adminToken,
			strings.NewReader(`{"principal":"admin-1","kind":"user","role":"collaborator"}`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("self-grant: expected 400, got %d", resp.StatusCode)
	}
}

func TestInvalidTokenReturns401(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	_, ts := testServerWithOIDC(t, idp)

	resp, _ := http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps", "invalid-token", nil))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid token, got %d", resp.StatusCode)
	}
}

func TestGrantAccessInvalidKind(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	adminToken := createTestPAT(t, srv.DB, "admin-1")

	// Create app
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", adminToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant with invalid kind
	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/access", adminToken,
			strings.NewReader(`{"principal":"user-2","kind":"invalid","role":"viewer"}`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestGrantAccessInvalidRole(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	adminToken := createTestPAT(t, srv.DB, "admin-1")

	// Create app
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", adminToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant with invalid role
	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/access", adminToken,
			strings.NewReader(`{"principal":"user-2","kind":"user","role":"superuser"}`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestGrantAccessEmptyPrincipal(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	adminToken := createTestPAT(t, srv.DB, "admin-1")

	// Create app
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", adminToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant with empty principal
	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/access", adminToken,
			strings.NewReader(`{"principal":"","kind":"user","role":"viewer"}`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestGrantAccessInvalidJSON(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	adminToken := createTestPAT(t, srv.DB, "admin-1")

	// Create app
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", adminToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant with bad JSON
	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/access", adminToken,
			strings.NewReader(`{not json`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRevokeAccessNonexistent(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	adminToken := createTestPAT(t, srv.DB, "admin-1")

	// Create app
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", adminToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Revoke access that doesn't exist
	resp, _ = http.DefaultClient.Do(
		jwtReq("DELETE", ts.URL+"/api/v1/apps/"+appID+"/access/user/nobody", adminToken, nil))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestNonAdminCannotGrantAccess(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	srv.DB.UpsertUserWithRole("viewer-1", "viewer@example.com", "Viewer", "viewer")

	// Admin creates app
	adminToken := createTestPAT(t, srv.DB, "admin-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", adminToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant viewer access so the viewer can see the app
	srv.DB.GrantAppAccess(appID, "viewer-1", "user", "viewer", "admin-1")

	// Viewer tries to grant access -> 404 (hidden)
	viewerToken := createTestPAT(t, srv.DB, "viewer-1")
	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/access", viewerToken,
			strings.NewReader(`{"principal":"user-2","kind":"user","role":"viewer"}`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for viewer granting access, got %d", resp.StatusCode)
	}
}

func TestNonAdminCannotRevokeAccess(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	srv.DB.UpsertUserWithRole("viewer-1", "viewer@example.com", "Viewer", "viewer")

	// Admin creates app and grants access to user-2
	adminToken := createTestPAT(t, srv.DB, "admin-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", adminToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	srv.DB.GrantAppAccess(appID, "user-2", "user", "viewer", "admin-1")
	srv.DB.GrantAppAccess(appID, "viewer-1", "user", "viewer", "admin-1")

	// Viewer tries to revoke access -> 404 (hidden)
	viewerToken := createTestPAT(t, srv.DB, "viewer-1")
	resp, _ = http.DefaultClient.Do(
		jwtReq("DELETE", ts.URL+"/api/v1/apps/"+appID+"/access/user/user-2", viewerToken, nil))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for viewer revoking access, got %d", resp.StatusCode)
	}
}

func TestPublicAppInUnfilteredList(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner-1", "owner@example.com", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("other-user", "other@example.com", "Other", "publisher")

	// Publisher creates app and makes it public
	ownerToken := createTestPAT(t, srv.DB, "owner-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"public-app"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	resp, _ = http.DefaultClient.Do(
		jwtReq("PATCH", ts.URL+"/api/v1/apps/"+appID, ownerToken,
			strings.NewReader(`{"access_type":"public"}`)))
	resp.Body.Close()

	// Another publisher (not owner, no grant) should see the public app
	otherToken := createTestPAT(t, srv.DB, "other-user")
	resp, _ = http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps", otherToken, nil))
	var apps []map[string]any
	json.NewDecoder(resp.Body).Decode(&apps)
	resp.Body.Close()

	found := false
	for _, a := range apps {
		if a["name"] == "public-app" {
			found = true
		}
	}
	if !found {
		t.Error("public app should be visible to other users")
	}
}

func TestViewerCannotStartApp(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner-1", "owner@example.com", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("viewer-1", "viewer@example.com", "Viewer", "viewer")

	// Publisher creates app
	ownerToken := createTestPAT(t, srv.DB, "owner-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant viewer access
	srv.DB.GrantAppAccess(appID, "viewer-1", "user", "viewer", "owner-1")

	// Viewer tries to start the app -> 404
	viewerToken := createTestPAT(t, srv.DB, "viewer-1")
	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/start", viewerToken, nil))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("viewer start: expected 404, got %d", resp.StatusCode)
	}
}

func TestViewerCannotStopApp(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner-1", "owner@example.com", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("viewer-1", "viewer@example.com", "Viewer", "viewer")

	// Publisher creates app
	ownerToken := createTestPAT(t, srv.DB, "owner-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant viewer access
	srv.DB.GrantAppAccess(appID, "viewer-1", "user", "viewer", "owner-1")

	// Viewer tries to stop the app -> 404
	viewerToken := createTestPAT(t, srv.DB, "viewer-1")
	resp, _ = http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/stop", viewerToken, nil))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("viewer stop: expected 404, got %d", resp.StatusCode)
	}
}

func TestCollaboratorCanUpdateConfig(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner-1", "owner@example.com", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("collab-1", "collab@example.com", "Collab", "publisher")

	// Owner creates app
	ownerToken := createTestPAT(t, srv.DB, "owner-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant collaborator access
	srv.DB.GrantAppAccess(appID, "collab-1", "user", "collaborator", "owner-1")

	// Collaborator updates memory_limit -> 200
	collabToken := createTestPAT(t, srv.DB, "collab-1")
	resp, _ = http.DefaultClient.Do(
		jwtReq("PATCH", ts.URL+"/api/v1/apps/"+appID, collabToken,
			strings.NewReader(`{"memory_limit":"512m"}`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("collaborator update config: expected 200, got %d", resp.StatusCode)
	}
}

func TestViewerCannotUpdateConfig(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner-1", "owner@example.com", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("viewer-1", "viewer@example.com", "Viewer", "viewer")

	// Owner creates app
	ownerToken := createTestPAT(t, srv.DB, "owner-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant viewer access
	srv.DB.GrantAppAccess(appID, "viewer-1", "user", "viewer", "owner-1")

	// Viewer tries to update memory_limit -> 404
	viewerToken := createTestPAT(t, srv.DB, "viewer-1")
	resp, _ = http.DefaultClient.Do(
		jwtReq("PATCH", ts.URL+"/api/v1/apps/"+appID, viewerToken,
			strings.NewReader(`{"memory_limit":"512m"}`)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("viewer update config: expected 404, got %d", resp.StatusCode)
	}
}

func TestNonAdminCannotDeleteTag(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@example.com", "Admin", "admin")
	srv.DB.UpsertUserWithRole("publisher-1", "pub@example.com", "Publisher", "publisher")

	// Admin creates a tag
	adminToken := createTestPAT(t, srv.DB, "admin-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/tags", adminToken,
			strings.NewReader(`{"name":"production"}`)))
	var tag map[string]any
	json.NewDecoder(resp.Body).Decode(&tag)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin create tag: expected 201, got %d", resp.StatusCode)
	}
	tagID := tag["id"].(string)

	// Publisher tries to delete the tag -> 404
	pubToken := createTestPAT(t, srv.DB, "publisher-1")
	resp, _ = http.DefaultClient.Do(
		jwtReq("DELETE", ts.URL+"/api/v1/tags/"+tagID, pubToken, nil))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("publisher delete tag: expected 404, got %d", resp.StatusCode)
	}
}

func TestViewerCanReadApp(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner-1", "owner@example.com", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("viewer-1", "viewer@example.com", "Viewer", "viewer")

	// Publisher creates app
	ownerToken := createTestPAT(t, srv.DB, "owner-1")
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"app-1"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant viewer access
	srv.DB.GrantAppAccess(appID, "viewer-1", "user", "viewer", "owner-1")

	// Viewer can GET the app -> 200
	viewerToken := createTestPAT(t, srv.DB, "viewer-1")
	resp, _ = http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps/"+appID, viewerToken, nil))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("viewer read app: expected 200, got %d", resp.StatusCode)
	}
}
