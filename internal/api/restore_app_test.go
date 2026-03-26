package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/testutil"
)

func TestRestoreApp(t *testing.T) {
	_, ts := testServerWithSoftDelete(t)
	created := createApp(t, ts, "restore-me")
	id := created["id"].(string)

	// Soft-delete.
	resp, _ := http.DefaultClient.Do(authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil))
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete: expected 204, got %d", resp.StatusCode)
	}

	// Restore.
	resp, err := http.DefaultClient.Do(authReq("POST", ts.URL+"/api/v1/apps/"+id+"/restore", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	if app["name"] != "restore-me" {
		t.Errorf("expected name 'restore-me', got %v", app["name"])
	}

	// App should be visible again.
	resp, _ = http.DefaultClient.Do(authReq("GET", ts.URL+"/api/v1/apps/"+id, nil))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 after restore, got %d", resp.StatusCode)
	}
}

func TestRestoreAppNotDeleted(t *testing.T) {
	_, ts := testServerWithSoftDelete(t)
	created := createApp(t, ts, "not-deleted")
	id := created["id"].(string)

	// Try to restore an app that isn't deleted.
	resp, err := http.DefaultClient.Do(authReq("POST", ts.URL+"/api/v1/apps/"+id+"/restore", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for non-deleted app, got %d", resp.StatusCode)
	}
}

func TestRestoreAppNonexistent(t *testing.T) {
	_, ts := testServerWithSoftDelete(t)

	resp, err := http.DefaultClient.Do(authReq("POST", ts.URL+"/api/v1/apps/nonexistent/restore", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRestoreAppNameConflict(t *testing.T) {
	_, ts := testServerWithSoftDelete(t)

	// Create and delete an app.
	created := createApp(t, ts, "conflict-name")
	id := created["id"].(string)
	resp, _ := http.DefaultClient.Do(authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil))
	resp.Body.Close()

	// Create another app with the same name.
	createApp(t, ts, "conflict-name")

	// Restore should fail with 409 — name already taken.
	resp, err := http.DefaultClient.Do(authReq("POST", ts.URL+"/api/v1/apps/"+id+"/restore", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
}

func TestRestoreAppNonOwnerPublisher(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)
	// Enable soft-delete.
	srv.Config.Storage.SoftDeleteRetention.Duration = 720 * 3600_000_000_000

	srv.DB.UpsertUserWithRole("owner", "owner@test", "Owner", "publisher")
	srv.DB.UpsertUserWithRole("other", "other@test", "Other", "publisher")
	ownerToken := createTestPAT(t, srv.DB, "owner")
	otherToken := createTestPAT(t, srv.DB, "other")

	// Owner creates and deletes an app.
	resp, _ := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps", ownerToken,
			strings.NewReader(`{"name":"restore-rbac"}`)))
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	appID := app["id"].(string)

	resp, _ = http.DefaultClient.Do(
		jwtReq("DELETE", ts.URL+"/api/v1/apps/"+appID, ownerToken, nil))
	resp.Body.Close()

	// Other publisher tries to restore — should get 404.
	resp, err := http.DefaultClient.Do(
		jwtReq("POST", ts.URL+"/api/v1/apps/"+appID+"/restore", otherToken, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRestoreAppUnauthenticated(t *testing.T) {
	_, ts := testServerWithSoftDelete(t)

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps/some-id/restore", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}
