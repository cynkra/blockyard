package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/testutil"
)

// --- ListAccess ---

func TestListAccessAsOwner(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner", "owner@test", "Owner", "publisher")
	ownerToken := createTestPAT(t, srv.DB, "owner")

	// Create app owned by "owner".
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"list-access-app"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant access to someone.
	srv.DB.GrantAppAccess(appID, "grantee", "user", "viewer", "owner")

	// List access — should succeed.
	resp, err := http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps/"+appID+"/access", ownerToken, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var grants []map[string]any
	json.NewDecoder(resp.Body).Decode(&grants)
	if len(grants) != 1 {
		t.Errorf("expected 1 grant, got %d", len(grants))
	}
	if grants[0]["principal"] != "grantee" {
		t.Errorf("expected principal 'grantee', got %v", grants[0]["principal"])
	}
}

func TestListAccessNonOwnerPublisher(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner", "owner@test", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("other", "other@test", "Other", "publisher")
	ownerToken := createTestPAT(t, srv.DB, "owner")
	otherToken := createTestPAT(t, srv.DB, "other")

	// Create app owned by "owner".
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"other-access-app"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Other publisher tries to list access — should get 404 (not owner).
	resp, err := http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps/"+appID+"/access", otherToken, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestListAccessNonexistentApp(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@test", "Admin", "admin")
	token := createTestPAT(t, srv.DB, "admin-1")

	resp, err := http.DefaultClient.Do(
		jwtReq("GET", ts.URL+"/api/v1/apps/nonexistent-id/access", token, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Update/Start/Stop by non-owner publisher ---

func TestNonOwnerCannotUpdateApp(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner", "owner@test", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("other", "other@test", "Other", "publisher")
	ownerToken := createTestPAT(t, srv.DB, "owner")
	otherToken := createTestPAT(t, srv.DB, "other")

	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"update-rbac-app"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Other publisher tries to update — should get 404.
	resp, err := http.DefaultClient.Do(
		jwtReq("PATCH", ts.URL+"/api/v1/apps/"+appID, otherToken,
			strings.NewReader(`{"title":"hacked"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestNonOwnerCannotStartApp(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner", "owner@test", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("other", "other@test", "Other", "publisher")
	ownerToken := createTestPAT(t, srv.DB, "owner")
	otherToken := createTestPAT(t, srv.DB, "other")

	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"start-rbac-app"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/start", otherToken, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestNonOwnerCannotStopApp(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner", "owner@test", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("other", "other@test", "Other", "publisher")
	ownerToken := createTestPAT(t, srv.DB, "owner")
	otherToken := createTestPAT(t, srv.DB, "other")

	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"stop-rbac-app"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/stop", otherToken, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Collaborator can start/stop but not delete or manage ACL ---

func TestCollaboratorCanStartButNotDelete(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner", "owner@test", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("collab", "collab@test", "Collab", "publisher")
	ownerToken := createTestPAT(t, srv.DB, "owner")
	collabToken := createTestPAT(t, srv.DB, "collab")

	// Create app.
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"collab-rbac-app"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	// Grant collaborator access.
	srv.DB.GrantAppAccess(appID, "collab", "user", "collaborator", "owner")

	// Collaborator can start (CanStartStop) — 409 proves auth passed.
	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/start", collabToken, nil))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 (no bundle), got %d", resp.StatusCode)
	}

	// Collaborator cannot delete (CanDelete requires owner).
	resp, err = http.DefaultClient.Do(
		jwtReq("DELETE", ts.URL+"/api/v1/apps/"+appID, collabToken, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for delete, got %d", resp.StatusCode)
	}
}

func TestCollaboratorCannotManageACL(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner", "owner@test", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("collab", "collab@test", "Collab", "publisher")
	ownerToken := createTestPAT(t, srv.DB, "owner")
	collabToken := createTestPAT(t, srv.DB, "collab")

	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"collab-acl-app"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	srv.DB.GrantAppAccess(appID, "collab", "user", "collaborator", "owner")

	// Collaborator cannot grant access (CanManageACL requires owner).
	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/access", collabToken,
			strings.NewReader(`{"principal":"someone","kind":"user","role":"viewer"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Rollback unauthenticated ---

func TestRollbackUnauthenticated(t *testing.T) {
	_, ts := testServer(t)

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps/some-id/rollback", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// --- Access endpoints with nonexistent app ---

func TestGrantAccessNonexistentApp(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@test", "Admin", "admin")
	token := createTestPAT(t, srv.DB, "admin-1")

	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/nonexistent/access", token,
			strings.NewReader(`{"principal":"someone","kind":"user","role":"viewer"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRevokeAccessNonexistentApp(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("admin-1", "admin@test", "Admin", "admin")
	token := createTestPAT(t, srv.DB, "admin-1")

	resp, err := http.DefaultClient.Do(
		jwtReq("DELETE", ts.URL+"/api/v1/apps/nonexistent/access/user/someone", token, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Self-grant prevention ---

func TestGrantAccessToSelf(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("owner", "owner@test", "Owner", "publisher")
	ownerToken := createTestPAT(t, srv.DB, "owner")

	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"self-grant-app"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/access", ownerToken,
			strings.NewReader(`{"principal":"owner","kind":"user","role":"viewer"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}
