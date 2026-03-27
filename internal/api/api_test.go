package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/testutil"
)

// testPAT is a fixed PAT used across tests. Created by testServer via
// seedTestAdmin.
const testPAT = "by_testtoken000000000000000000000000000000000"

func testServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	tmp := t.TempDir()

	cfg := &config.Config{
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, PakVersion: "stable"},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024, // 10 MiB for tests
		},
		Proxy: config.ProxyConfig{MaxWorkers: 100},
	}

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	seedTestAdmin(t, database)

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	// Track background restore goroutines so cleanup waits for them.
	var wg sync.WaitGroup
	srv.RestoreWG = &wg
	handler := NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	// Wait for restore goroutines before DB/TempDir cleanup (LIFO order).
	t.Cleanup(wg.Wait)

	return srv, ts
}

// seedTestAdmin creates an admin user and a PAT in the database for
// use in tests that authenticate via bearer token.
func seedTestAdmin(t *testing.T, database *db.DB) {
	t.Helper()
	_, err := database.UpsertUserWithRole("admin", "admin@test", "Admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	hash := auth.HashPAT(testPAT)
	_, err = database.CreatePAT("test-pat-id", hash, "admin", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
}

// authReq creates a request with the test PAT bearer token (admin).
func authReq(method, url string, body io.Reader) *http.Request {
	req, _ := http.NewRequest(method, url, body)
	req.Header.Set("Authorization", "Bearer "+testPAT)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// viewerPAT is a fixed PAT for a viewer-role user. Created by seedTestViewer.
const viewerPAT = "by_viewertoken0000000000000000000000000000000"

// seedTestViewer creates a viewer user and PAT.
func seedTestViewer(t *testing.T, database *db.DB) {
	t.Helper()
	_, err := database.UpsertUserWithRole("viewer", "viewer@test", "Viewer", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	hash := auth.HashPAT(viewerPAT)
	_, err = database.CreatePAT("viewer-pat-id", hash, "viewer", "viewer-test", nil)
	if err != nil {
		t.Fatal(err)
	}
}

// viewerReq creates a request with the viewer PAT bearer token.
func viewerReq(method, url string, body io.Reader) *http.Request {
	req, _ := http.NewRequest(method, url, body)
	req.Header.Set("Authorization", "Bearer "+viewerPAT)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// createApp is a test helper that creates an app via the API.
func createApp(t *testing.T, ts *httptest.Server, name string) map[string]interface{} {
	t.Helper()
	body := fmt.Sprintf(`{"name":"%s"}`, name)
	req := authReq("POST", ts.URL+"/api/v1/apps", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create app %q: expected 201, got %d: %s", name, resp.StatusCode, b)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result
}

// --- Existing tests ---

func TestHealthz(t *testing.T) {
	_, ts := testServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestUploadWithoutAuth(t *testing.T) {
	_, ts := testServer(t)
	app := createApp(t, ts, "test-app")

	resp, err := http.Post(
		ts.URL+"/api/v1/apps/"+app["id"].(string)+"/bundles",
		"application/octet-stream",
		bytes.NewReader(testutil.MakeBundle(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestUploadToNonexistentApp(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("POST", ts.URL+"/api/v1/apps/nonexistent/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestUploadBundleReturns202(t *testing.T) {
	_, ts := testServer(t)
	app := createApp(t, ts, "test-app")

	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+app["id"].(string)+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202, got %d", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["bundle_id"] == "" {
		t.Error("expected non-empty bundle_id")
	}
	if body["task_id"] == "" {
		t.Error("expected non-empty task_id")
	}
}

func TestTaskLogsStreamsOutput(t *testing.T) {
	_, ts := testServer(t)
	app := createApp(t, ts, "test-app")
	id := app["id"].(string)

	// Upload a bundle
	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	resp, _ := http.DefaultClient.Do(req)
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	taskID := body["task_id"]

	// Give the background goroutine a moment to run
	time.Sleep(100 * time.Millisecond)

	// Fetch task logs
	req = authReq("GET", ts.URL+"/api/v1/tasks/"+taskID+"/logs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	logs, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(logs), "restoring dependencies") {
		t.Errorf("expected restore log output, got: %s", logs)
	}
}

func TestListBundles(t *testing.T) {
	_, ts := testServer(t)
	app := createApp(t, ts, "test-app")
	id := app["id"].(string)

	// Upload two bundles
	for range 2 {
		req, _ := http.NewRequest("POST",
			ts.URL+"/api/v1/apps/"+id+"/bundles",
			bytes.NewReader(testutil.MakeBundle(t)))
		req.Header.Set("Authorization", "Bearer "+testPAT)
		http.DefaultClient.Do(req)
	}

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/bundles", nil)
	resp, _ := http.DefaultClient.Do(req)

	var envelope struct {
		Bundles []map[string]interface{} `json:"bundles"`
	}
	json.NewDecoder(resp.Body).Decode(&envelope)
	if len(envelope.Bundles) != 2 {
		t.Errorf("expected 2 bundles, got %d", len(envelope.Bundles))
	}
}

// --- App CRUD tests ---

func TestCreateApp(t *testing.T) {
	_, ts := testServer(t)
	result := createApp(t, ts, "my-app")

	if result["name"] != "my-app" {
		t.Errorf("expected name=my-app, got %v", result["name"])
	}
	if result["status"] != "stopped" {
		t.Errorf("expected status=stopped, got %v", result["status"])
	}
	if result["id"] == "" {
		t.Error("expected non-empty id")
	}
}

func TestCreateAppRejectsInvalidName(t *testing.T) {
	_, ts := testServer(t)
	for _, name := range []string{"My-App", "-app", "app-", "app_name", "1app"} {
		body := fmt.Sprintf(`{"name":"%s"}`, name)
		req := authReq("POST", ts.URL+"/api/v1/apps", strings.NewReader(body))
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("name %q: expected 400, got %d", name, resp.StatusCode)
		}
	}
}

func TestCreateDuplicateNameReturns409(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "my-app")

	body := `{"name":"my-app"}`
	req := authReq("POST", ts.URL+"/api/v1/apps", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409, got %d", resp.StatusCode)
	}
}

func TestListApps(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "app-a")
	createApp(t, ts, "app-b")

	req := authReq("GET", ts.URL+"/api/v1/apps", nil)
	resp, _ := http.DefaultClient.Do(req)

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var envelope struct {
		Apps []map[string]interface{} `json:"apps"`
	}
	json.NewDecoder(resp.Body).Decode(&envelope)
	if len(envelope.Apps) != 2 {
		t.Errorf("expected 2 apps, got %d", len(envelope.Apps))
	}
}

func TestGetAppByIDAndName(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Get by UUID
	req := authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 for UUID lookup, got %d", resp.StatusCode)
	}

	// Get by name
	req = authReq("GET", ts.URL+"/api/v1/apps/my-app", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 for name lookup, got %d", resp.StatusCode)
	}
}

func TestGetNonexistentAppReturns404(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("GET", ts.URL+"/api/v1/apps/nonexistent", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestUpdateApp(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"memory_limit":"512m"}`))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var updated map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&updated)
	if updated["memory_limit"] != "512m" {
		t.Errorf("expected memory_limit=512m, got %v", updated["memory_limit"])
	}
}

func TestUpdateAppRejectsInvalidSessionLimit(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// max_sessions_per_worker = 0 is invalid
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"max_sessions_per_worker":0}`))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for max_sessions_per_worker=0, got %d", resp.StatusCode)
	}

	// max_sessions_per_worker = 2 is now allowed
	req = authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"max_sessions_per_worker":2}`))
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for max_sessions_per_worker=2, got %d", resp.StatusCode)
	}

	// max_workers_per_app = 0 is invalid
	req = authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"max_workers_per_app":0}`))
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for max_workers_per_app=0, got %d", resp.StatusCode)
	}
}

func TestDeleteApp(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 204 {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}

	// Confirm gone
	req = authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404 after delete, got %d", resp.StatusCode)
	}
}

// --- App lifecycle tests ---

func TestEnableAppWithoutBundle(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/enable", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", body["enabled"])
	}
}

func TestEnableAndDisableApp(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Upload bundle and wait for restore
	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	http.DefaultClient.Do(req)
	time.Sleep(200 * time.Millisecond)

	// Enable
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/enable", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("enable: expected 200, got %d: %s", resp.StatusCode, b)
	}
	var enableBody map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&enableBody)
	if enableBody["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", enableBody["enabled"])
	}

	// Verify app is enabled via GET
	req = authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ = http.DefaultClient.Do(req)
	var appBody map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&appBody)
	if appBody["enabled"] != true {
		t.Errorf("expected app enabled=true, got %v", appBody["enabled"])
	}

	// Enable again — should be idempotent
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/enable", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 on second enable, got %d", resp.StatusCode)
	}

	// Simulate a running worker so disable has something to drain.
	srv.Workers.Set("test-worker", server.ActiveWorker{AppID: id})

	// Disable
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/disable", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("disable: expected 200, got %d: %s", resp.StatusCode, b)
	}
	var disableBody map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&disableBody)
	if disableBody["enabled"] != false {
		t.Errorf("expected enabled=false, got %v", disableBody["enabled"])
	}

	// Wait for async worker eviction.
	for i := 0; i < 50; i++ {
		if srv.Workers.Count() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if srv.Workers.Count() != 0 {
		t.Errorf("expected 0 workers after disable, got %d", srv.Workers.Count())
	}

	// Verify app is disabled via GET
	req = authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ = http.DefaultClient.Do(req)
	json.NewDecoder(resp.Body).Decode(&appBody)
	if appBody["enabled"] != false {
		t.Errorf("expected app enabled=false, got %v", appBody["enabled"])
	}
}

func TestDeleteAppStopsWorkers(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Simulate a running worker for this app.
	srv.Workers.Set("test-worker", server.ActiveWorker{AppID: id})
	if srv.Workers.Count() != 1 {
		t.Fatalf("expected 1 worker, got %d", srv.Workers.Count())
	}

	// Delete
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 204 {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}
	if srv.Workers.Count() != 0 {
		t.Errorf("expected 0 workers after delete, got %d", srv.Workers.Count())
	}
}

func TestEnableAppSucceedsRegardlessOfWorkerLimit(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		Server: config.ServerConfig{},
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, PakVersion: "stable"},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{MaxWorkers: 0}, // no workers allowed
	}
	database, _ := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)
	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	handler := NewRouter(srv)
	ts2 := httptest.NewServer(handler)
	t.Cleanup(ts2.Close)

	created := createApp(t, ts2, "my-app")
	id := created["id"].(string)

	// Enable just sets the flag — no worker spawning, so max_workers doesn't matter.
	req := authReq("POST", ts2.URL+"/api/v1/apps/"+id+"/enable", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", body["enabled"])
	}
}

func TestAppLogsMissingWorkerID(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/logs", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 without worker_id, got %d", resp.StatusCode)
	}
}

func TestAppLogsNotFound(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/logs?worker_id=nonexistent", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent worker, got %d", resp.StatusCode)
	}
}

func TestAppLogsReturnsBufferedLines(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Register worker so the IDOR check passes.
	srv.Workers.Set("w1", server.ActiveWorker{AppID: id})

	// Create log entry with some lines and mark ended
	sender := srv.LogStore.Create("w1", id)
	sender.Write("hello")
	sender.Write("world")
	srv.LogStore.MarkEnded("w1")

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/logs?worker_id=w1", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello\nworld\n" {
		t.Errorf("unexpected body: %q", string(body))
	}
}

// --- Task status tests ---

func TestGetTaskStatusRunning(t *testing.T) {
	srv, ts := testServer(t)

	sender := srv.Tasks.Create("task-1", "")
	sender.Write("line 1")

	req := authReq("GET", ts.URL+"/api/v1/tasks/task-1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["id"] != "task-1" {
		t.Errorf("expected id=task-1, got %v", body["id"])
	}
	if body["status"] != "running" {
		t.Errorf("expected status=running, got %v", body["status"])
	}
	if body["created_at"] == "" {
		t.Error("expected non-empty created_at")
	}

	// Clean up
	sender.Complete(task.Completed)
}

func TestGetTaskStatusCompleted(t *testing.T) {
	srv, ts := testServer(t)

	sender := srv.Tasks.Create("task-done", "")
	sender.Write("output")
	sender.Complete(task.Completed)

	req := authReq("GET", ts.URL+"/api/v1/tasks/task-done", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "completed" {
		t.Errorf("expected status=completed, got %v", body["status"])
	}
}

func TestGetTaskStatusFailed(t *testing.T) {
	srv, ts := testServer(t)

	sender := srv.Tasks.Create("task-fail", "")
	sender.Write("error output")
	sender.Complete(task.Failed)

	req := authReq("GET", ts.URL+"/api/v1/tasks/task-fail", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "failed" {
		t.Errorf("expected status=failed, got %v", body["status"])
	}
}

func TestGetTaskStatusNotFound(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("GET", ts.URL+"/api/v1/tasks/nonexistent", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Task logs tests ---

func TestTaskLogsNotFound(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("GET", ts.URL+"/api/v1/tasks/nonexistent/logs", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestTaskLogsCompletedTask(t *testing.T) {
	srv, ts := testServer(t)

	sender := srv.Tasks.Create("task-logs-done", "")
	sender.Write("line one")
	sender.Write("line two")
	sender.Complete(task.Completed)

	req := authReq("GET", ts.URL+"/api/v1/tasks/task-logs-done/logs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	expected := "line one\nline two\n"
	if string(body) != expected {
		t.Errorf("expected %q, got %q", expected, string(body))
	}
}

func TestTaskLogsFailedTask(t *testing.T) {
	srv, ts := testServer(t)

	sender := srv.Tasks.Create("task-logs-fail", "")
	sender.Write("starting")
	sender.Write("error: something went wrong")
	sender.Complete(task.Failed)

	req := authReq("GET", ts.URL+"/api/v1/tasks/task-logs-fail/logs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "error: something went wrong") {
		t.Errorf("expected error line in logs, got %q", string(body))
	}
}

func TestTaskLogsRunningTaskCompletes(t *testing.T) {
	srv, ts := testServer(t)

	sender := srv.Tasks.Create("task-logs-live", "")
	sender.Write("buffered line")

	// Complete the task in a goroutine after a short delay so the
	// streaming handler sees it transition from running to done.
	go func() {
		time.Sleep(50 * time.Millisecond)
		sender.Write("live line")
		time.Sleep(10 * time.Millisecond)
		sender.Complete(task.Completed)
	}()

	req := authReq("GET", ts.URL+"/api/v1/tasks/task-logs-live/logs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "buffered line") {
		t.Errorf("expected buffered line in output, got %q", body)
	}
	if !strings.Contains(string(body), "live line") {
		t.Errorf("expected live line in output, got %q", body)
	}
}

// --- CreateApp error path tests ---

func TestCreateAppInvalidJSON(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("POST", ts.URL+"/api/v1/apps", strings.NewReader(`{not json`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCreateAppMissingName(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("POST", ts.URL+"/api/v1/apps", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d", resp.StatusCode)
	}
}

// --- UpdateApp error path tests ---

func TestUpdateAppNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("PATCH", ts.URL+"/api/v1/apps/nonexistent",
		strings.NewReader(`{"memory_limit":"256m"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestUpdateAppInvalidJSON(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{not json`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestUpdateAppEmptyBody(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 200 for empty update, got %d: %s", resp.StatusCode, b)
	}
}

// --- DeleteApp error path tests ---

func TestDeleteAppNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("DELETE", ts.URL+"/api/v1/apps/nonexistent", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// --- DisableApp error path tests ---

func TestDisableAppNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("POST", ts.URL+"/api/v1/apps/nonexistent/disable", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestDisableAppNoRunningWorkers(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/disable", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["enabled"] != false {
		t.Errorf("expected enabled=false, got %v", body["enabled"])
	}
}

// --- App name validation unit tests ---

func TestValidateAppName(t *testing.T) {
	valid := []string{"a", "my-app", "app-123", "abc", "a1"}
	for _, name := range valid {
		if err := validateAppName(name); err != nil {
			t.Errorf("expected %q to be valid, got error: %v", name, err)
		}
	}

	invalid := []string{
		"",        // empty
		"A",       // uppercase
		"-app",    // starts with hyphen
		"app-",    // ends with hyphen
		"app_name", // underscore
		"1app",    // starts with digit
		strings.Repeat("a", 64), // too long
	}
	for _, name := range invalid {
		if err := validateAppName(name); err == nil {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}

// --- resolveApp unit test ---

func TestResolveAppByUUIDAndName(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Both UUID and name should resolve via the API (which uses resolveApp)
	for _, lookup := range []string{id, "my-app"} {
		req := authReq("GET", ts.URL+"/api/v1/apps/"+lookup, nil)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != 200 {
			t.Errorf("lookup %q: expected 200, got %d", lookup, resp.StatusCode)
		}
		var app map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&app)
		if app["id"] != id {
			t.Errorf("lookup %q: expected id=%s, got %v", lookup, id, app["id"])
		}
	}
}

// --- UploadBundle error path tests ---

func TestUploadBundleOversized(t *testing.T) {
	// Create a server with a very small max bundle size
	tmp := t.TempDir()
	cfg := &config.Config{
		Server: config.ServerConfig{},
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, PakVersion: "stable"},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10, // 10 bytes — any real bundle will exceed this
		},
		Proxy: config.ProxyConfig{MaxWorkers: 100},
	}
	database, _ := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)
	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	handler := NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", resp.StatusCode)
	}
}

func TestUploadBundleMissingEntrypoint(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Create a tar.gz without app.R
	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundleWithoutEntrypoint(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing entrypoint, got %d", resp.StatusCode)
	}
}

func TestListBundlesNonexistentApp(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("GET", ts.URL+"/api/v1/apps/nonexistent/bundles", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestEnableAppNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("POST", ts.URL+"/api/v1/apps/nonexistent/enable", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Bundle endpoints use resolveApp ---

func TestUploadBundleByName(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "my-app")

	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/my-app/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202, got %d", resp.StatusCode)
	}
}

// --- Audit log integration tests ---

// testServerWithAudit creates a test server with an active audit log.
func testServerWithAudit(t *testing.T) (*server.Server, *httptest.Server, string) {
	t.Helper()
	tmp := t.TempDir()
	auditPath := tmp + "/audit.jsonl"

	cfg := &config.Config{
		Server:  config.ServerConfig{},
		Docker:  config.DockerConfig{Image: "test-image", ShinyPort: 3838, PakVersion: "stable"},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{MaxWorkers: 100},
		Audit: &config.AuditConfig{Path: auditPath},
	}

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	var wg sync.WaitGroup
	srv.RestoreWG = &wg

	// Create and start audit log.
	auditLog := audit.New(auditPath)
	srv.AuditLog = auditLog
	ctx, cancel := context.WithCancel(context.Background())
	go auditLog.Run(ctx, auditPath)
	t.Cleanup(cancel)

	handler := NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	t.Cleanup(wg.Wait)

	return srv, ts, auditPath
}

func TestAuditLogCapturesAppCRUD(t *testing.T) {
	_, ts, auditPath := testServerWithAudit(t)

	// Create app
	app := createApp(t, ts, "audited-app")
	appID := app["id"].(string)

	// Update app
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+appID,
		strings.NewReader(`{"memory_limit":"256m"}`))
	http.DefaultClient.Do(req)

	// Delete app
	req = authReq("DELETE", ts.URL+"/api/v1/apps/"+appID, nil)
	http.DefaultClient.Do(req)

	// Give the background writer time to flush.
	time.Sleep(100 * time.Millisecond)

	// Read audit log and check for entries.
	data, err := io.ReadAll(openFile(t, auditPath))
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 audit entries, got %d: %s", len(lines), string(data))
	}

	actions := make([]string, len(lines))
	for i, line := range lines {
		var entry map[string]any
		json.Unmarshal([]byte(line), &entry)
		actions[i] = entry["action"].(string)
	}

	// Verify we have create, update, delete actions.
	found := map[string]bool{}
	for _, a := range actions {
		found[a] = true
	}
	for _, expected := range []string{"app.create", "app.update", "app.delete"} {
		if !found[expected] {
			t.Errorf("missing audit action %q in %v", expected, actions)
		}
	}
}

func openFile(t *testing.T, path string) io.Reader {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// --- Tracing middleware integration test ---

func TestTracingMiddlewareEnabled(t *testing.T) {
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
		Telemetry: &config.TelemetryConfig{
			MetricsEnabled: true,
			OTLPEndpoint:   "localhost:4317", // triggers middleware registration
		},
	}

	database, _ := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)
	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	handler := NewRouter(srv)

	// Healthz should work even with tracing middleware enabled.
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	// Metrics endpoint should be available (requires auth).
	req = httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+testPAT)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /metrics, got %d", rec.Code)
	}
}

// --- Helper function unit tests ---

func TestValidateTagName(t *testing.T) {
	valid := []string{"a", "my-tag", "tag-123", "abc", "a1"}
	for _, name := range valid {
		if err := validateTagName(name); err != nil {
			t.Errorf("expected %q to be valid, got error: %v", name, err)
		}
	}
	invalid := []string{
		"",                       // empty
		"A",                      // uppercase
		"-tag",                   // starts with hyphen
		"tag-",                   // ends with hyphen
		"tag_name",               // underscore
		"1tag",                   // starts with digit
		strings.Repeat("a", 64), // too long
	}
	for _, name := range invalid {
		if err := validateTagName(name); err == nil {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}

func TestStringOrEmpty(t *testing.T) {
	if got := stringOrEmpty(nil); got != "" {
		t.Errorf("stringOrEmpty(nil) = %q, want empty", got)
	}
	s := "hello"
	if got := stringOrEmpty(&s); got != "hello" {
		t.Errorf("stringOrEmpty(&s) = %q, want hello", got)
	}
}

func TestFloatOrZero(t *testing.T) {
	if got := floatOrZero(nil); got != 0 {
		t.Errorf("floatOrZero(nil) = %f, want 0", got)
	}
	f := 3.14
	if got := floatOrZero(&f); got != 3.14 {
		t.Errorf("floatOrZero(&f) = %f, want 3.14", got)
	}
}

func TestParseIntOr(t *testing.T) {
	if got := parseIntOr("", 42); got != 42 {
		t.Errorf("parseIntOr empty = %d, want 42", got)
	}
	if got := parseIntOr("abc", 42); got != 42 {
		t.Errorf("parseIntOr invalid = %d, want 42", got)
	}
	if got := parseIntOr("7", 42); got != 7 {
		t.Errorf("parseIntOr valid = %d, want 7", got)
	}
}

func TestClampEdgeCases(t *testing.T) {
	if got := clamp(0, 1, 100); got != 1 {
		t.Errorf("clamp below = %d, want 1", got)
	}
	if got := clamp(200, 1, 100); got != 100 {
		t.Errorf("clamp above = %d, want 100", got)
	}
	if got := clamp(50, 1, 100); got != 50 {
		t.Errorf("clamp within = %d, want 50", got)
	}
}

// --- Additional UpdateApp error path tests ---

func TestUpdateAppMaxWorkersInvalid(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"max_workers_per_app":-1}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for max_workers_per_app=-1, got %d", resp.StatusCode)
	}
}

func TestUpdateAppInvalidAccessType(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"access_type":"invalid"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 400 for access_type=invalid, got %d: %s", resp.StatusCode, b)
	}
}

func TestUpdateAppSetDescription(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"title":"My App","description":"A test app"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var updated map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&updated)
	if updated["title"] != "My App" {
		t.Errorf("expected title=My App, got %v", updated["title"])
	}
	if updated["description"] != "A test app" {
		t.Errorf("expected description=A test app, got %v", updated["description"])
	}
}

// --- CreateApp additional error paths ---

func TestCreateAppRejectsEmptyName(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("POST", ts.URL+"/api/v1/apps", strings.NewReader(`{"name":""}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for empty name, got %d", resp.StatusCode)
	}
}

// --- DeleteApp with bundles ---

func TestDeleteAppWithBundles(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Upload a bundle
	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("upload: expected 202, got %d", resp.StatusCode)
	}

	// Wait for restore to finish
	time.Sleep(200 * time.Millisecond)

	// Verify bundles exist
	req = authReq("GET", ts.URL+"/api/v1/apps/"+id+"/bundles", nil)
	resp, _ = http.DefaultClient.Do(req)
	var bundleEnvelope struct {
		Bundles []map[string]interface{} `json:"bundles"`
	}
	json.NewDecoder(resp.Body).Decode(&bundleEnvelope)
	if len(bundleEnvelope.Bundles) == 0 {
		t.Fatal("expected at least 1 bundle before delete")
	}

	// Delete the app
	req = authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 204, got %d: %s", resp.StatusCode, b)
	}

	// Confirm the app is gone
	req = authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", resp.StatusCode)
	}
}

// --- DisableApp already disabled path ---

func TestDisableAppAlreadyDisabled(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Disable an app that was never enabled — should return 200 with enabled=false
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/disable", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["enabled"] != false {
		t.Errorf("expected enabled=false, got %v", body["enabled"])
	}

	// Call disable again — idempotent
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/disable", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 on second disable, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["enabled"] != false {
		t.Errorf("expected enabled=false on second disable, got %v", body["enabled"])
	}
}

// --- StartApp spawn failure ---

// faultyBackend wraps mock.MockBackend to inject errors into Spawn and Addr.
type faultyBackend struct {
	*mock.MockBackend
	spawnErr error
}

func (f *faultyBackend) Spawn(ctx context.Context, spec backend.WorkerSpec) error {
	if f.spawnErr != nil {
		return f.spawnErr
	}
	return f.MockBackend.Spawn(ctx, spec)
}

func testServerWithBackend(t *testing.T, be backend.Backend) (*server.Server, *httptest.Server) {
	t.Helper()
	tmp := t.TempDir()

	cfg := &config.Config{
		Server: config.ServerConfig{},
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, PakVersion: "stable"},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{MaxWorkers: 100},
	}

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)

	srv := server.NewServer(cfg, be, database)
	handler := NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return srv, ts
}

func TestEnableAppSetsEnabledFlag(t *testing.T) {
	fb := &faultyBackend{
		MockBackend: mock.New(),
		spawnErr:    fmt.Errorf("container runtime unavailable"),
	}
	_, ts := testServerWithBackend(t, fb)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Enable just toggles the flag — no worker spawning, so backend errors don't matter.
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/enable", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", body["enabled"])
	}
}

// --- AppLogs IDOR: worker belongs to a different app ---

func TestAppLogsWorkerBelongsToDifferentApp(t *testing.T) {
	srv, ts := testServer(t)
	appA := createApp(t, ts, "app-a")
	createApp(t, ts, "app-b")
	idA := appA["id"].(string)

	// Register a worker for app-b, but query logs via app-a.
	srv.Workers.Set("w-other", server.ActiveWorker{AppID: "some-other-app-id"})

	req := authReq("GET", ts.URL+"/api/v1/apps/"+idA+"/logs?worker_id=w-other", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for IDOR check, got %d", resp.StatusCode)
	}
}

// --- AppLogs: worker exists for app but LogStore has no entry ---

func TestAppLogsNoLogsInStore(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Register a worker for the app, but don't create a LogStore entry.
	srv.Workers.Set("w-nologs", server.ActiveWorker{AppID: id})

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/logs?worker_id=w-nologs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for worker with no logs, got %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] != "not_found" {
		t.Errorf("expected error=not_found, got %q", body["error"])
	}
	if !strings.Contains(body["message"], "no logs") {
		t.Errorf("expected message about no logs, got %q", body["message"])
	}
}

// --- AppLogs: streaming live lines then channel closes ---

func TestAppLogsStreamingLiveLines(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Register worker so IDOR check passes.
	srv.Workers.Set("w-live", server.ActiveWorker{AppID: id})

	// Create log entry with a buffered line (not ended yet, so streaming path is taken).
	sender := srv.LogStore.Create("w-live", id)
	sender.Write("buffered-line")

	// In a goroutine, write a live line then end the stream.
	go func() {
		time.Sleep(50 * time.Millisecond)
		sender.Write("live-line")
		time.Sleep(10 * time.Millisecond)
		srv.LogStore.MarkEnded("w-live")
	}()

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/logs?worker_id=w-live", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	output := string(body)
	if !strings.Contains(output, "buffered-line") {
		t.Errorf("expected buffered-line in output, got %q", output)
	}
	if !strings.Contains(output, "live-line") {
		t.Errorf("expected live-line in output, got %q", output)
	}
}

// --- UpdateApp: valid access_type values ---

func TestUpdateAppAccessTypeValues(t *testing.T) {
	_, ts := testServer(t)

	names := map[string]string{"acl": "app-acl", "logged_in": "app-loggedin", "public": "app-public"}
	for _, at := range []string{"acl", "logged_in", "public"} {
		created := createApp(t, ts, names[at])
		id := created["id"].(string)

		body := fmt.Sprintf(`{"access_type":"%s"}`, at)
		req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("access_type=%q: expected 200, got %d: %s", at, resp.StatusCode, b)
			continue
		}
		var updated map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&updated)
		if updated["access_type"] != at {
			t.Errorf("expected access_type=%s, got %v", at, updated["access_type"])
		}
	}
}

// --- DeleteApp by name ---

func TestDeleteAppByName(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "del-by-name")

	req := authReq("DELETE", ts.URL+"/api/v1/apps/del-by-name", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}

	// Confirm gone.
	req = authReq("GET", ts.URL+"/api/v1/apps/del-by-name", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 after delete by name, got %d", resp.StatusCode)
	}
}

// --- EnableApp by name ---

func TestEnableAppByName(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "enable-name")

	req := authReq("POST", ts.URL+"/api/v1/apps/enable-name/enable", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", body["enabled"])
	}
}

// --- DisableApp by name ---

func TestDisableAppByName(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "disable-name")

	req := authReq("POST", ts.URL+"/api/v1/apps/disable-name/disable", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

// --- AppLogs: context cancellation during streaming ---

func TestAppLogsStreamingContextCancel(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	srv.Workers.Set("w-cancel", server.ActiveWorker{AppID: id})
	sender := srv.LogStore.Create("w-cancel", id)
	sender.Write("first-line")

	// Use a context with short timeout to exercise the ctx.Done() path
	// in the streaming select loop (line 652).
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET",
		ts.URL+"/api/v1/apps/"+id+"/logs?worker_id=w-cancel", nil)
	req.Header.Set("Authorization", "Bearer "+testPAT)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Context cancellation may cause a transport error — that's expected.
		return
	}
	// If we got a response, it should be 200 and contain the buffered line.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "first-line") {
		t.Errorf("expected first-line in output, got %q", body)
	}
}

// --- Task and audit handler coverage tests ---

func TestGetTaskStatus(t *testing.T) {
	srv, ts := testServer(t)

	sender := srv.Tasks.Create("cov-running", "")
	sender.Write("hello from task")

	req := authReq("GET", ts.URL+"/api/v1/tasks/cov-running", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["id"] != "cov-running" {
		t.Errorf("expected id=cov-running, got %v", body["id"])
	}
	if body["status"] != "running" {
		t.Errorf("expected status=running, got %v", body["status"])
	}
	if body["created_at"] == "" {
		t.Error("expected non-empty created_at")
	}

	sender.Complete(task.Completed)
}

func TestGetTaskStatusNotFound2(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("GET", ts.URL+"/api/v1/tasks/nonexistent", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestGetTaskStatusCompleted2(t *testing.T) {
	srv, ts := testServer(t)

	sender := srv.Tasks.Create("cov-completed", "")
	sender.Write("some output")
	sender.Complete(task.Completed)

	req := authReq("GET", ts.URL+"/api/v1/tasks/cov-completed", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "completed" {
		t.Errorf("expected status=completed, got %v", body["status"])
	}
}

func TestTaskLogsSnapshot(t *testing.T) {
	srv, ts := testServer(t)

	sender := srv.Tasks.Create("cov-logs-snap", "")
	sender.Write("log line 1")
	sender.Write("log line 2")
	sender.Write("log line 3")
	sender.Complete(task.Completed)

	req := authReq("GET", ts.URL+"/api/v1/tasks/cov-logs-snap/logs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	expected := "log line 1\nlog line 2\nlog line 3\n"
	if string(body) != expected {
		t.Errorf("expected %q, got %q", expected, string(body))
	}
}

func TestTaskLogsNotFound2(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("GET", ts.URL+"/api/v1/tasks/nonexistent/logs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestTaskLogsClientDisconnect(t *testing.T) {
	srv, ts := testServer(t)
	sender := srv.Tasks.Create("task-disconnect", "")
	sender.Write("initial line")

	// Use a context with cancel to simulate client disconnect.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET",
		ts.URL+"/api/v1/tasks/task-disconnect/logs", nil)
	req.Header.Set("Authorization", "Bearer "+testPAT)

	// Start request in a goroutine since it will block.
	done := make(chan struct{})
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		close(done)
	}()

	// Give time for the handler to start streaming, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Wait for the client goroutine to finish.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client to finish")
	}

	// Cleanup: complete the task so it doesn't leak.
	sender.Complete(task.Completed)
}

func TestTaskLogsWithAppAccess(t *testing.T) {
	srv, ts := testServer(t)

	// Create a task associated with an app.
	created := createApp(t, ts, "my-app")
	appID := created["id"].(string)
	sender := srv.Tasks.Create("task-with-app", appID)
	sender.Write("app-specific log")
	sender.Complete(task.Completed)

	req := authReq("GET", ts.URL+"/api/v1/tasks/task-with-app/logs", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "app-specific log") {
		t.Errorf("expected 'app-specific log' in output, got %q", body)
	}
}

// --- Rollback tests ---

func TestRollbackAppValidBundle(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Create two ready bundles.
	srv.DB.CreateBundle("b-1", id, "", false)
	srv.DB.UpdateBundleStatus("b-1", "ready")
	srv.DB.SetActiveBundle(id, "b-1")

	srv.DB.CreateBundle("b-2", id, "", false)
	srv.DB.UpdateBundleStatus("b-2", "ready")

	// Rollback to b-2.
	body := `{"bundle_id":"b-2"}`
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["active_bundle"] != "b-2" {
		t.Errorf("expected active_bundle=b-2, got %v", result["active_bundle"])
	}
}

func TestRollbackToNonexistentBundle(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	body := `{"bundle_id":"nonexistent"}`
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRollbackToBundleOfDifferentApp(t *testing.T) {
	srv, ts := testServer(t)
	app1 := createApp(t, ts, "app-one")
	id1 := app1["id"].(string)
	app2 := createApp(t, ts, "app-two")
	id2 := app2["id"].(string)

	srv.DB.CreateBundle("b-other", id2, "", false)
	srv.DB.UpdateBundleStatus("b-other", "ready")

	body := `{"bundle_id":"b-other"}`
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id1+"/rollback", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRollbackToFailedBundle(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	srv.DB.CreateBundle("b-fail", id, "", false)
	srv.DB.UpdateBundleStatus("b-fail", "failed")

	body := `{"bundle_id":"b-fail"}`
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRollbackToAlreadyActiveBundle(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	srv.DB.CreateBundle("b-1", id, "", false)
	srv.DB.UpdateBundleStatus("b-1", "ready")
	srv.DB.SetActiveBundle(id, "b-1")

	body := `{"bundle_id":"b-1"}`
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRollbackWithoutBundleID(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	body := `{}`
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRollbackStopsRunningWorkers(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Create an active bundle and a second ready bundle.
	srv.DB.CreateBundle("b-1", id, "", false)
	srv.DB.UpdateBundleStatus("b-1", "ready")
	srv.DB.SetActiveBundle(id, "b-1")

	// Simulate a running worker for this app.
	srv.Workers.Set("test-worker", server.ActiveWorker{AppID: id, BundleID: "b-1"})
	if srv.Workers.Count() != 1 {
		t.Fatalf("expected 1 worker, got %d", srv.Workers.Count())
	}

	// Create a second ready bundle.
	srv.DB.CreateBundle("b-rollback", id, "", false)
	srv.DB.UpdateBundleStatus("b-rollback", "ready")

	// Rollback to the new bundle.
	body := `{"bundle_id":"b-rollback"}`
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	// Workers should be stopped.
	if srv.Workers.Count() != 0 {
		t.Errorf("expected 0 workers after rollback, got %d", srv.Workers.Count())
	}

	// Active bundle should be changed.
	app, _ := srv.DB.GetApp(id)
	if app.ActiveBundle == nil || *app.ActiveBundle != "b-rollback" {
		t.Errorf("expected active bundle b-rollback, got %v", app.ActiveBundle)
	}
}

// --- Soft-delete API tests ---

func testServerWithSoftDelete(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	tmp := t.TempDir()

	cfg := &config.Config{
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, PakVersion: "stable"},
		Storage: config.StorageConfig{
			BundleServerPath:    tmp,
			BundleWorkerPath:    "/app",
			BundleRetention:     50,
			MaxBundleSize:       10 * 1024 * 1024,
			SoftDeleteRetention: config.Duration{Duration: 720 * time.Hour},
		},
		Proxy: config.ProxyConfig{MaxWorkers: 100},
	}

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	seedTestAdmin(t, database)

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	handler := NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return srv, ts
}

func TestSoftDeleteApp(t *testing.T) {
	srv, ts := testServerWithSoftDelete(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Delete (soft).
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 204 {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}

	// App should be gone from listings.
	req = authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}

	// But still in DB with deleted_at set.
	app, _ := srv.DB.GetAppIncludeDeleted(id)
	if app == nil {
		t.Fatal("expected app to still be in DB")
	}
	if app.DeletedAt == nil {
		t.Error("expected deleted_at to be set")
	}
}

func TestHardDeleteAppWhenSoftDeleteDisabled(t *testing.T) {
	srv, ts := testServer(t) // no soft_delete_retention
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 204 {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}

	// Should be fully gone.
	app, _ := srv.DB.GetAppIncludeDeleted(id)
	if app != nil {
		t.Error("expected app to be completely removed")
	}
}

func TestRestoreAppAPI(t *testing.T) {
	_, ts := testServerWithSoftDelete(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Soft delete.
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	http.DefaultClient.Do(req)

	// Restore.
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/restore", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	// App should be back in listings.
	req = authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestRestoreWithNameCollisionAPI(t *testing.T) {
	_, ts := testServerWithSoftDelete(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Soft delete.
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	http.DefaultClient.Do(req)

	// Create another app with the same name.
	createApp(t, ts, "my-app")

	// Restore should fail with 409.
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/restore", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 409 {
		t.Errorf("expected 409, got %d", resp.StatusCode)
	}
}

func TestRestoreNonexistentApp(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("POST", ts.URL+"/api/v1/apps/nonexistent/restore", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRestoreNonDeletedApp(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/restore", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestListDeletedAppsAdmin(t *testing.T) {
	_, ts := testServerWithSoftDelete(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)
	createApp(t, ts, "live-app")

	// Soft delete one app.
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	http.DefaultClient.Do(req)

	// List deleted apps.
	req = authReq("GET", ts.URL+"/api/v1/apps?deleted=true", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var apps []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&apps)
	if len(apps) != 1 {
		t.Fatalf("expected 1 deleted app, got %d", len(apps))
	}
	if apps[0]["id"] != id {
		t.Errorf("expected deleted app id=%s, got %v", id, apps[0]["id"])
	}
}

func TestListDeletedAppsNonAdminForbidden(t *testing.T) {
	srv, ts := testServerWithSoftDelete(t)
	seedTestViewer(t, srv.DB)

	req := viewerReq("GET", ts.URL+"/api/v1/apps?deleted=true", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 403 {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestRollbackWithoutDeployPermission(t *testing.T) {
	srv, ts := testServer(t)
	seedTestViewer(t, srv.DB)

	// Admin creates app and bundles.
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)
	srv.DB.CreateBundle("b-1", id, "", false)
	srv.DB.UpdateBundleStatus("b-1", "ready")
	srv.DB.SetActiveBundle(id, "b-1")
	srv.DB.CreateBundle("b-2", id, "", false)
	srv.DB.UpdateBundleStatus("b-2", "ready")

	// Viewer tries to rollback → 404 (permission denied, masked as not found).
	body := `{"bundle_id":"b-2"}`
	req := viewerReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRestoreWithoutPermission(t *testing.T) {
	srv, ts := testServerWithSoftDelete(t)
	seedTestViewer(t, srv.DB)

	// Admin creates and deletes an app.
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	http.DefaultClient.Do(req)

	// Viewer tries to restore → 404 (permission denied, masked as not found).
	req = viewerReq("POST", ts.URL+"/api/v1/apps/"+id+"/restore", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Resource limit validation tests ---

func TestUpdateAppInvalidMemoryLimit(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	body := `{"memory_limit":"banana"}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestUpdateAppNegativeCPULimit(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	body := `{"cpu_limit":-1}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestUpdateAppCPULimitExceedsCeiling(t *testing.T) {
	tmp := t.TempDir()
	maxCPU := 4.0
	cfg := &config.Config{
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, PakVersion: "stable"},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{
			MaxWorkers:  100,
			MaxCPULimit: &maxCPU,
		},
	}
	database, _ := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)
	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	handler := NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	body := `{"cpu_limit":5.0}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}

	// Within ceiling should succeed.
	body = `{"cpu_limit":2.0}`
	req = authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

func TestUpdateAppCPULimitCeilingDisabled(t *testing.T) {
	tmp := t.TempDir()
	zeroCPU := 0.0
	cfg := &config.Config{
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, PakVersion: "stable"},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{
			MaxWorkers:  100,
			MaxCPULimit: &zeroCPU,
		},
	}
	database, _ := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)
	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	handler := NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Any positive value should be accepted.
	body := `{"cpu_limit":100.0}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

func TestUpdateAppValidResourceLimits(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	body := `{"memory_limit":"512m","cpu_limit":2.0}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["memory_limit"] != "512m" {
		t.Errorf("expected memory_limit=512m, got %v", result["memory_limit"])
	}
	if result["cpu_limit"] != 2.0 {
		t.Errorf("expected cpu_limit=2, got %v", result["cpu_limit"])
	}
}

func TestUpdateAppEmptyMemoryLimitAccepted(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Empty string clears the limit — should not be validated.
	body := `{"memory_limit":""}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

func TestUpdateAppPreWarmedSeats(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"pre_warmed_seats":1}`))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var updated map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&updated)
	if v := updated["pre_warmed_seats"]; v != float64(1) {
		t.Errorf("expected pre_warmed_seats=1, got %v", v)
	}
}

func TestUpdateAppPreWarmedSeatsNegative(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"pre_warmed_seats":-1}`))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestUpdateAppPreWarmedSeatsExceedsCap(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"pre_warmed_seats":11}`))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestGetAppIncludesPreWarmedSeats(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ := http.DefaultClient.Do(req)
	var app map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&app)

	if _, ok := app["pre_warmed_seats"]; !ok {
		t.Error("expected pre_warmed_seats in response")
	}
	if v := app["pre_warmed_seats"]; v != float64(0) {
		t.Errorf("expected pre_warmed_seats=0, got %v", v)
	}
}

// --- Audit log coverage for rollback and restore ---

func TestRollbackAppAuditLog(t *testing.T) {
	srv, ts, auditPath := testServerWithAudit(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Create two ready bundles and activate the first.
	srv.DB.CreateBundle("b-1", id, "", false)
	srv.DB.UpdateBundleStatus("b-1", "ready")
	srv.DB.SetActiveBundle(id, "b-1")
	srv.DB.CreateBundle("b-2", id, "", false)
	srv.DB.UpdateBundleStatus("b-2", "ready")

	// Rollback to b-2.
	body := `{"bundle_id":"b-2"}`
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	// Check audit log contains rollback entry.
	time.Sleep(100 * time.Millisecond)
	data, _ := io.ReadAll(openFile(t, auditPath))
	if !strings.Contains(string(data), "app.rollback") {
		t.Errorf("expected app.rollback in audit log, got:\n%s", data)
	}
	if !strings.Contains(string(data), "previous_bundle_id") {
		t.Errorf("expected previous_bundle_id in audit log")
	}
}

func testServerWithSoftDeleteAndAudit(t *testing.T) (*server.Server, *httptest.Server, string) {
	t.Helper()
	tmp := t.TempDir()
	auditPath := tmp + "/audit.jsonl"

	cfg := &config.Config{
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, PakVersion: "stable"},
		Storage: config.StorageConfig{
			BundleServerPath:    tmp,
			BundleWorkerPath:    "/app",
			BundleRetention:     50,
			MaxBundleSize:       10 * 1024 * 1024,
			SoftDeleteRetention: config.Duration{Duration: 720 * time.Hour},
		},
		Proxy: config.ProxyConfig{MaxWorkers: 100},
		Audit: &config.AuditConfig{Path: auditPath},
	}

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	auditLog := audit.New(auditPath)
	srv.AuditLog = auditLog
	ctx, cancel := context.WithCancel(context.Background())
	go auditLog.Run(ctx, auditPath)
	t.Cleanup(cancel)

	handler := NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return srv, ts, auditPath
}

func TestRestoreAppAuditLog(t *testing.T) {
	_, ts, auditPath := testServerWithSoftDeleteAndAudit(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Soft delete.
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	http.DefaultClient.Do(req)

	// Restore.
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/restore", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	// Check audit log contains restore entry.
	time.Sleep(100 * time.Millisecond)
	data, _ := io.ReadAll(openFile(t, auditPath))
	if !strings.Contains(string(data), "app.restore") {
		t.Errorf("expected app.restore in audit log, got:\n%s", data)
	}
}

func TestRollbackAppInvalidJSON(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback",
		strings.NewReader("not json"))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRollbackAppPendingBundle(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Create a pending bundle (default status).
	srv.DB.CreateBundle("b-pending", id, "", false)

	body := `{"bundle_id":"b-pending"}`
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/rollback", strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// --- Upload bundle edge cases ---

func TestUploadBundleTooLarge(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Send a body that exceeds MaxBundleSize (10 MiB).
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/bundles",
		strings.NewReader(strings.Repeat("x", 11*1024*1024)))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 413 {
		t.Errorf("expected 413, got %d", resp.StatusCode)
	}
}

func TestUploadBundleInvalidArchive(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/bundles",
		strings.NewReader("not a tar.gz"))
	resp, _ := http.DefaultClient.Do(req)
	// Should fail during unpack with 500.
	if resp.StatusCode != 500 {
		t.Errorf("expected 500, got %d", resp.StatusCode)
	}
}

func TestUploadBundleNoDeployPermission(t *testing.T) {
	srv, ts := testServer(t)
	seedTestViewer(t, srv.DB)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := viewerReq("POST", ts.URL+"/api/v1/apps/"+id+"/bundles",
		strings.NewReader("data"))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Runtime endpoint tests ---

func TestGetAppRuntimeAdmin(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "runtime-app")
	id := created["id"].(string)

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/runtime", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)

	// Workers should be an empty array.
	workers, ok := body["workers"].([]interface{})
	if !ok {
		t.Fatal("expected workers array")
	}
	if len(workers) != 0 {
		t.Errorf("expected 0 workers, got %d", len(workers))
	}

	// Metrics should be present.
	if _, ok := body["active_sessions"]; !ok {
		t.Error("expected active_sessions field")
	}
	if _, ok := body["total_views"]; !ok {
		t.Error("expected total_views field")
	}
}

func TestGetAppRuntimeViewerDenied(t *testing.T) {
	srv, ts := testServer(t)
	seedTestViewer(t, srv.DB)

	created := createApp(t, ts, "runtime-denied")
	id := created["id"].(string)

	req := viewerReq("GET", ts.URL+"/api/v1/apps/"+id+"/runtime", nil)
	resp, _ := http.DefaultClient.Do(req)
	// Viewer cannot access runtime — should get 404 (opaque denial).
	if resp.StatusCode != 404 {
		t.Errorf("expected 404 for viewer, got %d", resp.StatusCode)
	}
}

func TestGetAppRuntimeNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("GET", ts.URL+"/api/v1/apps/nonexistent/runtime", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Sessions endpoint tests ---

func TestListAppSessions(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "sessions-app")
	id := created["id"].(string)

	// Insert sessions directly into the DB.
	srv.DB.CreateSession("api-s1", id, "w1", "user-a")
	srv.DB.CreateSession("api-s2", id, "w1", "user-b")

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/sessions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body struct {
		Sessions []map[string]interface{} `json:"sessions"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(body.Sessions))
	}
}

func TestListAppSessionsViewerDenied(t *testing.T) {
	srv, ts := testServer(t)
	seedTestViewer(t, srv.DB)
	created := createApp(t, ts, "sessions-denied")
	id := created["id"].(string)

	req := viewerReq("GET", ts.URL+"/api/v1/apps/"+id+"/sessions", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404 for viewer, got %d", resp.StatusCode)
	}
}

// --- Per-app tags endpoint tests ---

func TestListAppTags(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "tagged-app")
	id := created["id"].(string)

	// Create a tag and assign it.
	tag, err := srv.DB.CreateTag("my-tag")
	if err != nil {
		t.Fatal(err)
	}
	srv.DB.AddAppTag(id, tag.ID)

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/tags", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body struct {
		Tags []map[string]interface{} `json:"tags"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Tags) != 1 {
		t.Errorf("expected 1 tag, got %d", len(body.Tags))
	}
	if len(body.Tags) > 0 && body.Tags[0]["name"] != "my-tag" {
		t.Errorf("expected tag name=my-tag, got %v", body.Tags[0]["name"])
	}
}

func TestListAppTagsNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("GET", ts.URL+"/api/v1/apps/nonexistent/tags", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Users/me endpoint tests ---

func TestGetCurrentUser(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("GET", ts.URL+"/api/v1/users/me", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["sub"] != "admin" {
		t.Errorf("expected sub=admin, got %v", body["sub"])
	}
	if body["role"] != "admin" {
		t.Errorf("expected role=admin, got %v", body["role"])
	}
}

func TestGetCurrentUserUnauthenticated(t *testing.T) {
	_, ts := testServer(t)

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/me", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// --- Deployments endpoint tests ---

func TestListDeploymentsAPI(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "deploy-api-app")
	id := created["id"].(string)

	// Upload a bundle so we have a deployment.
	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	http.DefaultClient.Do(req)
	time.Sleep(200 * time.Millisecond)

	// Mark bundle as deployed by setting deployed_at.
	bundles, _ := srv.DB.ListBundlesByApp(id)
	if len(bundles) > 0 {
		now := time.Now().UTC().Format(time.RFC3339)
		srv.DB.Exec(`UPDATE bundles SET deployed_at = ? WHERE id = ?`,
			now, bundles[0].ID)
	}

	req = authReq("GET", ts.URL+"/api/v1/deployments", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body struct {
		Deployments []map[string]interface{} `json:"deployments"`
		Total       int                      `json:"total"`
		Page        int                      `json:"page"`
		PerPage     int                      `json:"per_page"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Page != 1 {
		t.Errorf("expected page=1, got %d", body.Page)
	}
	if body.PerPage != 25 {
		t.Errorf("expected per_page=25, got %d", body.PerPage)
	}
}

func TestListDeploymentsUnauthenticated(t *testing.T) {
	_, ts := testServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/deployments", nil)
	resp, _ := http.DefaultClient.Do(req)
	// Unauthenticated should be rejected (401 from auth middleware).
	if resp.StatusCode != 401 {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// --- Purge endpoint tests ---

func TestPurgeAppAdmin(t *testing.T) {
	srv, ts := testServerWithSoftDelete(t)
	created := createApp(t, ts, "purge-api-app")
	id := created["id"].(string)

	// Soft-delete first.
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 204 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("soft-delete: expected 204, got %d: %s", resp.StatusCode, b)
	}

	// Verify it's soft-deleted (still in DB).
	fetched, _ := srv.DB.GetAppIncludeDeleted(id)
	if fetched == nil {
		t.Fatal("expected app to exist after soft-delete")
	}

	// Purge (admin + soft-deleted).
	req = authReq("DELETE", ts.URL+"/api/v1/apps/"+id+"?purge=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 204 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("purge: expected 204, got %d: %s", resp.StatusCode, b)
	}

	// Confirm completely gone.
	fetched, _ = srv.DB.GetAppIncludeDeleted(id)
	if fetched != nil {
		t.Error("expected app to be completely gone after purge")
	}
}

func TestPurgeAppNotSoftDeleted(t *testing.T) {
	_, ts := testServerWithSoftDelete(t)
	created := createApp(t, ts, "purge-active")
	id := created["id"].(string)

	// Try to purge without soft-deleting first.
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id+"?purge=true", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 409 {
		t.Errorf("expected 409 for purge of active app, got %d", resp.StatusCode)
	}
}

func TestPurgeAppViewerDenied(t *testing.T) {
	srv, ts := testServerWithSoftDelete(t)
	seedTestViewer(t, srv.DB)
	created := createApp(t, ts, "purge-viewer")
	id := created["id"].(string)

	// Soft-delete first (as admin).
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	http.DefaultClient.Do(req)

	// Try to purge as viewer.
	req = viewerReq("DELETE", ts.URL+"/api/v1/apps/"+id+"?purge=true", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 403 {
		t.Errorf("expected 403 for viewer purge, got %d", resp.StatusCode)
	}
}

func TestPurgeAppNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("DELETE", ts.URL+"/api/v1/apps/nonexistent?purge=true", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// --- ListAppsV2 endpoint tests ---

func TestListAppsV2Paginated(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "v2-a")
	createApp(t, ts, "v2-b")
	createApp(t, ts, "v2-c")

	req := authReq("GET", ts.URL+"/api/v1/apps?per_page=2&page=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body struct {
		Apps    []map[string]interface{} `json:"apps"`
		Total   int                      `json:"total"`
		Page    int                      `json:"page"`
		PerPage int                      `json:"per_page"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Total != 3 {
		t.Errorf("expected total=3, got %d", body.Total)
	}
	if body.Page != 1 {
		t.Errorf("expected page=1, got %d", body.Page)
	}
	if body.PerPage != 2 {
		t.Errorf("expected per_page=2, got %d", body.PerPage)
	}
	if len(body.Apps) != 2 {
		t.Errorf("expected 2 apps on page 1, got %d", len(body.Apps))
	}

	// Each app should have relation, status, tags fields.
	for _, app := range body.Apps {
		if _, ok := app["relation"]; !ok {
			t.Error("expected relation field in app")
		}
		if _, ok := app["status"]; !ok {
			t.Error("expected status field in app")
		}
		if _, ok := app["tags"]; !ok {
			t.Error("expected tags field in app")
		}
	}
}

func TestListAppsV2DeletedAdmin(t *testing.T) {
	_, ts := testServerWithSoftDelete(t)
	created := createApp(t, ts, "v2-deleted")
	id := created["id"].(string)

	// Soft-delete (retention > 0, so this is a soft delete).
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 204 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("soft-delete: expected 204, got %d: %s", resp.StatusCode, b)
	}

	// List deleted apps.
	req = authReq("GET", ts.URL+"/api/v1/apps?deleted=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if len(body) != 1 {
		t.Errorf("expected 1 deleted app, got %d", len(body))
	}
}

func TestListAppsV2DeletedViewerDenied(t *testing.T) {
	srv, ts := testServer(t)
	seedTestViewer(t, srv.DB)

	req := viewerReq("GET", ts.URL+"/api/v1/apps?deleted=true", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 403 {
		t.Errorf("expected 403 for viewer listing deleted, got %d", resp.StatusCode)
	}
}

func TestListAppsV2SearchFilter(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "dashboard-search")
	createApp(t, ts, "plumber-search")

	req := authReq("GET", ts.URL+"/api/v1/apps?search=dashboard", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body struct {
		Apps  []map[string]interface{} `json:"apps"`
		Total int                      `json:"total"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Total != 1 {
		t.Errorf("expected total=1, got %d", body.Total)
	}
}

func TestListAppsV2TagFilter(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "tagged-list")
	createApp(t, ts, "untagged-list")
	id := created["id"].(string)

	tag, _ := srv.DB.CreateTag("v2-tag")
	srv.DB.AddAppTag(id, tag.ID)

	req := authReq("GET", ts.URL+"/api/v1/apps?tag=v2-tag", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body struct {
		Apps  []map[string]interface{} `json:"apps"`
		Total int                      `json:"total"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Total != 1 {
		t.Errorf("expected total=1, got %d", body.Total)
	}
}

func TestListAppsV2Unauthenticated(t *testing.T) {
	_, ts := testServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/apps", nil)
	resp, _ := http.DefaultClient.Do(req)
	// Unauthenticated is rejected by the auth middleware.
	if resp.StatusCode != 401 {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestGetCurrentUserWithDBUser(t *testing.T) {
	srv, ts := testServer(t)

	// The admin user already has a DB row (from seedTestAdmin).
	// Verify we get the email field.
	req := authReq("GET", ts.URL+"/api/v1/users/me", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["email"] != "admin@test" {
		t.Errorf("expected email=admin@test, got %v", body["email"])
	}
	if body["name"] != "Admin" {
		t.Errorf("expected name=Admin, got %v", body["name"])
	}
	_ = srv // just to use srv
}

func TestGetCurrentUserViewerPAT(t *testing.T) {
	srv, ts := testServer(t)
	seedTestViewer(t, srv.DB)

	req := viewerReq("GET", ts.URL+"/api/v1/users/me", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["sub"] != "viewer" {
		t.Errorf("expected sub=viewer, got %v", body["sub"])
	}
	if body["role"] != "viewer" {
		t.Errorf("expected role=viewer, got %v", body["role"])
	}
}

func TestGetAppRuntimeWithSessions(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "runtime-sess")
	id := created["id"].(string)

	// Create DB sessions for metrics.
	srv.DB.CreateSession("rs1", id, "w1", "user-a")
	srv.DB.CreateSession("rs2", id, "w1", "user-b")
	srv.DB.CreateSession("rs3", id, "w1", "user-a") // duplicate user

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/runtime", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)

	totalViews := int(body["total_views"].(float64))
	if totalViews != 3 {
		t.Errorf("expected total_views=3, got %d", totalViews)
	}
	uniqueVisitors := int(body["unique_visitors"].(float64))
	if uniqueVisitors != 2 {
		t.Errorf("expected unique_visitors=2, got %d", uniqueVisitors)
	}
}

func TestListAppSessionsWithFilter(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "sess-filter")
	id := created["id"].(string)

	srv.DB.CreateSession("sf1", id, "w1", "user-a")
	srv.DB.CreateSession("sf2", id, "w1", "user-b")
	srv.DB.EndSession("sf1", "ended")

	// Filter by status.
	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/sessions?status=active", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body struct {
		Sessions []map[string]interface{} `json:"sessions"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Sessions) != 1 {
		t.Errorf("expected 1 active session, got %d", len(body.Sessions))
	}
}

func TestListDeploymentsWithSearch(t *testing.T) {
	srv, ts := testServer(t)
	createApp(t, ts, "deploy-search-a")
	createApp(t, ts, "deploy-search-b")

	// Create a deployed bundle for app-a only.
	apps, _ := srv.DB.ListApps()
	for _, app := range apps {
		if app.Name == "deploy-search-a" {
			srv.DB.CreateBundle("ds-b1", app.ID, "", false)
			now := time.Now().UTC().Format(time.RFC3339)
			srv.DB.Exec(`UPDATE bundles SET deployed_at = ?, status = 'active' WHERE id = ?`,
				now, "ds-b1")
		}
	}

	req := authReq("GET", ts.URL+"/api/v1/deployments?search=search-a", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body struct {
		Deployments []map[string]interface{} `json:"deployments"`
		Total       int                      `json:"total"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Total != 1 {
		t.Errorf("expected total=1, got %d", body.Total)
	}
}
