package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/testutil"
)

func testServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	tmp := t.TempDir()

	cfg := &config.Config{
		Server: config.ServerConfig{Token: "test-token"},
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838},
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

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	handler := NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return srv, ts
}

// authReq creates a request with the test bearer token.
func authReq(method, url string, body io.Reader) *http.Request {
	req, _ := http.NewRequest(method, url, body)
	req.Header.Set("Authorization", "Bearer test-token")
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
	req.Header.Set("Authorization", "Bearer test-token")

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
	req.Header.Set("Authorization", "Bearer test-token")
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
		req.Header.Set("Authorization", "Bearer test-token")
		http.DefaultClient.Do(req)
	}

	// Give restore goroutines time to finish
	time.Sleep(100 * time.Millisecond)

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

func TestUpdateAppRejectsMultiSession(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"max_sessions_per_worker":2}`))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
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
	req.Header.Set("Authorization", "Bearer test-token")
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

	// Stop
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/stop", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var stopBody map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&stopBody)
	if stopBody["workers_stopped"] != float64(1) {
		t.Errorf("expected workers_stopped=1, got %v", stopBody["workers_stopped"])
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
	req.Header.Set("Authorization", "Bearer test-token")
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
		Server: config.ServerConfig{Token: "test-token"},
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838},
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

func TestAppLogsReturns501(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/logs", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("expected 501, got %d", resp.StatusCode)
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

// --- Bundle endpoints use resolveApp ---

func TestUploadBundleByName(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "my-app")

	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/my-app/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer test-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202, got %d", resp.StatusCode)
	}
}
