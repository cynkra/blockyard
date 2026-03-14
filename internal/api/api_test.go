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
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, RvBinaryPath: testutil.FakeRvBinary(t)},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024, // 10 MiB for tests
		},
		Proxy: config.ProxyConfig{MaxWorkers: 100},
	}

	database, err := db.Open(":memory:")
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

// authReq creates a request with the test PAT bearer token.
func authReq(method, url string, body io.Reader) *http.Request {
	req, _ := http.NewRequest(method, url, body)
	req.Header.Set("Authorization", "Bearer "+testPAT)
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
	if !strings.Contains(string(logs), "Starting dependency restoration") {
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

	var bundles []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&bundles)
	if len(bundles) != 2 {
		t.Errorf("expected 2 bundles, got %d", len(bundles))
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
	var apps []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&apps)
	if len(apps) != 2 {
		t.Errorf("expected 2 apps, got %d", len(apps))
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

func TestStartAppWithoutBundleReturnsConflict(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/start", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409, got %d", resp.StatusCode)
	}
}

func TestStartAndStopApp(t *testing.T) {
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

	// Start
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/start", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("start: expected 200, got %d: %s", resp.StatusCode, b)
	}
	var startBody map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&startBody)
	if startBody["status"] != "running" {
		t.Errorf("expected status=running, got %v", startBody["status"])
	}
	if startBody["worker_id"] == "" {
		t.Error("expected non-empty worker_id")
	}

	if srv.Workers.Count() != 1 {
		t.Errorf("expected 1 worker, got %d", srv.Workers.Count())
	}

	// Verify app status is "running"
	req = authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ = http.DefaultClient.Do(req)
	var appBody map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&appBody)
	if appBody["status"] != "running" {
		t.Errorf("expected app status=running, got %v", appBody["status"])
	}

	// Start again — should be no-op, return same worker
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/start", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 on second start, got %d", resp.StatusCode)
	}
	if srv.Workers.Count() != 1 {
		t.Errorf("expected still 1 worker, got %d", srv.Workers.Count())
	}

	// Stop — returns 202 with task_id for async drain
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/stop", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 202 {
		t.Errorf("expected 202, got %d", resp.StatusCode)
	}
	var stopBody map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&stopBody)
	if stopBody["task_id"] == nil || stopBody["task_id"] == "" {
		t.Error("expected non-empty task_id")
	}
	if stopBody["worker_count"] != float64(1) {
		t.Errorf("expected worker_count=1, got %v", stopBody["worker_count"])
	}

	// Wait for async drain to complete.
	taskID := stopBody["task_id"].(string)
	for i := 0; i < 50; i++ {
		st, ok := srv.Tasks.Status(taskID)
		if ok && st != task.Running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if srv.Workers.Count() != 0 {
		t.Errorf("expected 0 workers, got %d", srv.Workers.Count())
	}

	// Verify app status is "stopped"
	req = authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ = http.DefaultClient.Do(req)
	json.NewDecoder(resp.Body).Decode(&appBody)
	if appBody["status"] != "stopped" {
		t.Errorf("expected app status=stopped, got %v", appBody["status"])
	}
}

func TestDeleteAppStopsWorkers(t *testing.T) {
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

	// Start
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/start", nil)
	http.DefaultClient.Do(req)
	if srv.Workers.Count() != 1 {
		t.Fatalf("expected 1 worker, got %d", srv.Workers.Count())
	}

	// Delete
	req = authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 204 {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}
	if srv.Workers.Count() != 0 {
		t.Errorf("expected 0 workers after delete, got %d", srv.Workers.Count())
	}
}

func TestStartAtMaxWorkersReturns503(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		Server: config.ServerConfig{},
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, RvBinaryPath: testutil.FakeRvBinary(t)},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{MaxWorkers: 0}, // no workers allowed
	}
	database, _ := db.Open(":memory:")
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)
	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	handler := NewRouter(srv)
	ts2 := httptest.NewServer(handler)
	t.Cleanup(ts2.Close)

	created := createApp(t, ts2, "my-app")
	id := created["id"].(string)

	// Set an active bundle directly to bypass the upload flow
	srv.DB.CreateBundle("b-1", id)
	srv.DB.UpdateBundleStatus("b-1", "ready")
	srv.DB.SetActiveBundle(id, "b-1")

	req := authReq("POST", ts2.URL+"/api/v1/apps/"+id+"/start", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
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

	sender := srv.Tasks.Create("task-1")
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

	sender := srv.Tasks.Create("task-done")
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

	sender := srv.Tasks.Create("task-fail")
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

	sender := srv.Tasks.Create("task-logs-done")
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

	sender := srv.Tasks.Create("task-logs-fail")
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

	sender := srv.Tasks.Create("task-logs-live")
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

// --- StopApp error path tests ---

func TestStopAppNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("POST", ts.URL+"/api/v1/apps/nonexistent/stop", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestStopAppNoRunningWorkers(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/stop", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["stopped_workers"] != float64(0) {
		t.Errorf("expected stopped_workers=0, got %v", body["stopped_workers"])
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
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, RvBinaryPath: testutil.FakeRvBinary(t)},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10, // 10 bytes — any real bundle will exceed this
		},
		Proxy: config.ProxyConfig{MaxWorkers: 100},
	}
	database, _ := db.Open(":memory:")
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

func TestStartAppNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("POST", ts.URL+"/api/v1/apps/nonexistent/start", nil)
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
		Docker:  config.DockerConfig{Image: "test-image", ShinyPort: 3838, RvBinaryPath: testutil.FakeRvBinary(t)},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{MaxWorkers: 100},
		Audit: &config.AuditConfig{Path: auditPath},
	}

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)

	be := mock.New()
	srv := server.NewServer(cfg, be, database)

	// Create and start audit log.
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

	database, _ := db.Open(":memory:")
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
	var bundles []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&bundles)
	if len(bundles) == 0 {
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

// --- StopApp already stopped path ---

func TestStopAppAlreadyStopped(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Stop an app that was never started — should return 200 with stopped_workers=0
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/stop", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["stopped_workers"] != float64(0) {
		t.Errorf("expected stopped_workers=0, got %v", body["stopped_workers"])
	}

	// Call stop again — same result
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/stop", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 on second stop, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["stopped_workers"] != float64(0) {
		t.Errorf("expected stopped_workers=0 on second stop, got %v", body["stopped_workers"])
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
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, RvBinaryPath: testutil.FakeRvBinary(t)},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{MaxWorkers: 100},
	}

	database, err := db.Open(":memory:")
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

func TestStartAppSpawnError(t *testing.T) {
	fb := &faultyBackend{
		MockBackend: mock.New(),
		spawnErr:    fmt.Errorf("container runtime unavailable"),
	}
	srv, ts := testServerWithBackend(t, fb)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Set an active bundle directly to bypass the upload flow.
	srv.DB.CreateBundle("b-1", id)
	srv.DB.UpdateBundleStatus("b-1", "ready")
	srv.DB.SetActiveBundle(id, "b-1")

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500 on spawn error, got %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] != "internal_error" {
		t.Errorf("expected error=internal_error, got %q", body["error"])
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

// --- StartApp by name ---

func TestStartAppByName(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "start-name")
	id := created["id"].(string)

	// Set an active bundle directly.
	srv.DB.CreateBundle("b-name", id)
	srv.DB.UpdateBundleStatus("b-name", "ready")
	srv.DB.SetActiveBundle(id, "b-name")

	req := authReq("POST", ts.URL+"/api/v1/apps/start-name/start", nil)
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
	if body["status"] != "running" {
		t.Errorf("expected status=running, got %v", body["status"])
	}
}

// --- StopApp by name ---

func TestStopAppByName(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "stop-name")

	req := authReq("POST", ts.URL+"/api/v1/apps/stop-name/stop", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
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
