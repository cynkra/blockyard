package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/pkgstore"
	"github.com/cynkra/blockyard/internal/server"
)

// testServerWithPkgStore returns a test server that also has PkgStore
// initialized (needed for refresh/rollback tests that launch background tasks).
func testServerWithPkgStore(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	srv, ts := testServer(t)
	storeRoot := filepath.Join(srv.Config.Storage.BundleServerPath, ".pkg-store")
	os.MkdirAll(storeRoot, 0o755)
	srv.PkgStore = pkgstore.NewStore(storeRoot)
	return srv, ts
}

func TestPostRefresh_NoActiveBundle(t *testing.T) {
	_, ts := testServer(t)

	app := createApp(t, ts, "refresh-test-1")
	appID := app["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/refresh", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 409, got %d: %s", resp.StatusCode, body)
	}
}

func TestPostRefresh_PinnedDeployment(t *testing.T) {
	srv, ts := testServer(t)

	app := createApp(t, ts, "refresh-pinned")
	appID := app["id"].(string)

	bundleID := "test-bundle-pinned"
	srv.DB.CreateBundle(bundleID, appID)
	srv.DB.UpdateBundleStatus(bundleID, "active")
	srv.DB.SetActiveBundle(appID, bundleID)

	bundlePaths := srv.BundlePaths(appID, bundleID)
	os.MkdirAll(bundlePaths.Base, 0o755)
	m := &manifest.Manifest{
		Version:  1,
		Metadata: manifest.Metadata{Entrypoint: "app.R"},
		Packages: map[string]manifest.Package{
			"shiny": {Package: "shiny", Version: "1.9.1", Source: "Repository"},
		},
	}
	m.Write(filepath.Join(bundlePaths.Base, "manifest.json"))

	req := authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/refresh", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 409 for pinned deployment, got %d: %s", resp.StatusCode, body)
	}
}

func TestPostRefresh_UnpinnedStartsTask(t *testing.T) {
	srv, ts := testServerWithPkgStore(t)

	app := createApp(t, ts, "refresh-unpinned")
	appID := app["id"].(string)

	bundleID := "test-bundle-unpinned"
	srv.DB.CreateBundle(bundleID, appID)
	srv.DB.UpdateBundleStatus(bundleID, "active")
	srv.DB.SetActiveBundle(appID, bundleID)

	bundlePaths := srv.BundlePaths(appID, bundleID)
	os.MkdirAll(bundlePaths.Base, 0o755)
	m := &manifest.Manifest{
		Version:     1,
		Metadata:    manifest.Metadata{Entrypoint: "app.R"},
		Description: map[string]string{"Imports": "shiny"},
	}
	m.Write(filepath.Join(bundlePaths.Base, "manifest.json"))

	req := authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/refresh", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 202, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["task_id"] == "" {
		t.Error("expected task_id in response")
	}
}

func TestPostRefreshRollback_NoActiveBundle(t *testing.T) {
	_, ts := testServer(t)

	app := createApp(t, ts, "rollback-test-1")
	appID := app["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/refresh/rollback", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 409, got %d: %s", resp.StatusCode, body)
	}
}

func TestPostRefreshRollback_NoPrevManifest(t *testing.T) {
	srv, ts := testServer(t)

	app := createApp(t, ts, "rollback-no-prev")
	appID := app["id"].(string)

	bundleID := "test-bundle-rollback"
	srv.DB.CreateBundle(bundleID, appID)
	srv.DB.UpdateBundleStatus(bundleID, "active")
	srv.DB.SetActiveBundle(appID, bundleID)

	bundlePaths := srv.BundlePaths(appID, bundleID)
	os.MkdirAll(bundlePaths.Base, 0o755)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/refresh/rollback", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 409 (no prev manifest), got %d: %s", resp.StatusCode, body)
	}
}

func TestPostRefreshRollback_WithPrevManifest(t *testing.T) {
	srv, ts := testServerWithPkgStore(t)

	app := createApp(t, ts, "rollback-with-prev")
	appID := app["id"].(string)

	bundleID := "test-bundle-rollback-prev"
	srv.DB.CreateBundle(bundleID, appID)
	srv.DB.UpdateBundleStatus(bundleID, "active")
	srv.DB.SetActiveBundle(appID, bundleID)

	bundlePaths := srv.BundlePaths(appID, bundleID)
	os.MkdirAll(bundlePaths.Base, 0o755)

	pkgstore.WriteStoreManifest(bundlePaths.Base, map[string]string{"shiny": "v1/c1"})
	prevDir := t.TempDir()
	pkgstore.WriteStoreManifest(prevDir, map[string]string{"shiny": "v0/c0"})
	data, _ := os.ReadFile(filepath.Join(prevDir, "store-manifest.json"))
	os.WriteFile(filepath.Join(bundlePaths.Base, "store-manifest.json.prev"), data, 0o644)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/refresh/rollback", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 202, got %d: %s", resp.StatusCode, body)
	}

	// Wait for the background rollback task to complete before test cleanup.
	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if taskID := result["task_id"]; taskID != "" {
		for i := 0; i < 50; i++ {
			time.Sleep(50 * time.Millisecond)
			if s, ok := srv.Tasks.Status(taskID); ok && s != 0 { // not Running
				break
			}
		}
	}
}

func TestPostRefreshRollback_BuildTarget_NoManifest(t *testing.T) {
	srv, ts := testServer(t)

	app := createApp(t, ts, "rollback-build-none")
	appID := app["id"].(string)

	bundleID := "test-bundle-build-rollback"
	srv.DB.CreateBundle(bundleID, appID)
	srv.DB.UpdateBundleStatus(bundleID, "active")
	srv.DB.SetActiveBundle(appID, bundleID)

	bundlePaths := srv.BundlePaths(appID, bundleID)
	os.MkdirAll(bundlePaths.Base, 0o755)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/refresh/rollback?target=build", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 409 (no build manifest), got %d: %s", resp.StatusCode, body)
	}
}

func TestPostPackages_InvalidJSON(t *testing.T) {
	srv, ts := testServer(t)

	key := testWorkerSigningKey()
	srv.WorkerTokenKey = key

	token := makeWorkerToken(t, key, "worker:w1", "app-1", "w1", 15*60*1e9)

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/packages",
		strings.NewReader("not json"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 400 for invalid JSON, got %d: %s", resp.StatusCode, body)
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]string{"hello": "world"})

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var result map[string]string
	json.NewDecoder(rec.Body).Decode(&result)
	if result["hello"] != "world" {
		t.Errorf("body = %v, want hello=world", result)
	}
}

func TestWriteJSON_Error(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusNotFound, map[string]string{"message": "not found"})

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
