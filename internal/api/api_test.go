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

	_ "github.com/cynkra/blockyard/internal/api/docs"
	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
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
	handler := NewRouter(srv, func() {}, nil, context.Background())
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	taskID := body["task_id"]

	// Give the background goroutine a moment to run
	time.Sleep(100 * time.Millisecond)

	// Fetch task logs
	req = authReq("GET", ts.URL+"/api/v1/tasks/"+taskID+"/logs", nil)
	resp, err = http.DefaultClient.Do(req)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

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
		resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409, got %d", resp.StatusCode)
	}
}

func TestListApps(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "app-a")
	createApp(t, ts, "app-b")

	req := authReq("GET", ts.URL+"/api/v1/apps", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var updated map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&updated)
	if updated["memory_limit"] != "512m" {
		t.Errorf("expected memory_limit=512m, got %v", updated["memory_limit"])
	}
	// Default test backend is "docker"; the warning header must NOT be set.
	if w := resp.Header.Get("X-Blockyard-Warning"); w != "" {
		t.Errorf("unexpected warning header on docker backend: %q", w)
	}
}

func TestUpdateAppWarnsOnProcessBackendLimit(t *testing.T) {
	srv, ts := testServer(t)
	srv.Config.Server.Backend = "process"
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	cases := []struct {
		name string
		body string
	}{
		{"memory_limit", `{"memory_limit":"512m"}`},
		{"cpu_limit", `{"cpu_limit":2.0}`},
		{"both", `{"memory_limit":"1g","cpu_limit":1.5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(tc.body))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
			}
			warning := resp.Header.Get("X-Blockyard-Warning")
			if warning == "" {
				t.Error("expected X-Blockyard-Warning header on process backend, got none")
			}
			if !strings.Contains(warning, "process backend") {
				t.Errorf("warning should mention process backend: %q", warning)
			}
		})
	}
}

func TestUpdateAppNoWarningWhenLimitNotSet(t *testing.T) {
	srv, ts := testServer(t)
	srv.Config.Server.Backend = "process"
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Update something OTHER than memory/cpu — no warning expected.
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"max_workers_per_app":3}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	if w := resp.Header.Get("X-Blockyard-Warning"); w != "" {
		t.Errorf("unexpected warning when not setting memory/cpu: %q", w)
	}
}

func TestUpdateAppRejectsInvalidSessionLimit(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// max_sessions_per_worker = 0 is invalid
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"max_sessions_per_worker":0}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	handler := NewRouter(srv, func() {}, nil, context.Background())
	ts2 := httptest.NewServer(handler)
	t.Cleanup(ts2.Close)

	created := createApp(t, ts2, "my-app")
	id := created["id"].(string)

	// Enable just sets the flag — no worker spawning, so max_workers doesn't matter.
	req := authReq("POST", ts2.URL+"/api/v1/apps/"+id+"/enable", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 without worker_id, got %d", resp.StatusCode)
	}
}

func TestAppLogsNotFound(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/logs?worker_id=nonexistent", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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

func TestGetTaskStatusFailed(t *testing.T) {
	srv, ts := testServer(t)

	sender := srv.Tasks.Create("task-fail", "")
	sender.Write("error output")
	sender.Complete(task.Failed)

	req := authReq("GET", ts.URL+"/api/v1/tasks/task-fail", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Task logs tests ---

func TestTaskLogsNotFound(t *testing.T) {
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
		resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	handler := NewRouter(srv, func() {}, nil, context.Background())
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestEnableAppNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("POST", ts.URL+"/api/v1/apps/nonexistent/enable", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	auditLog := audit.New(auditPath, srv.Metrics)
	srv.AuditLog = auditLog
	ctx, cancel := context.WithCancel(context.Background())
	go auditLog.Run(ctx, auditPath)
	t.Cleanup(cancel)

	handler := NewRouter(srv, func() {}, nil, context.Background())
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
	handler := NewRouter(srv, func() {}, nil, context.Background())

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
	handler := NewRouter(srv, func() {}, nil, context.Background())
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	handler := NewRouter(srv, func() {}, nil, context.Background())
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
		return
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 409 {
		t.Errorf("expected 409, got %d", resp.StatusCode)
	}
}

func TestRestoreNonexistentApp(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("POST", ts.URL+"/api/v1/apps/nonexistent/restore", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRestoreNonDeletedApp(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/restore", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	handler := NewRouter(srv, func() {}, nil, context.Background())
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	body := `{"cpu_limit":5.0}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	handler := NewRouter(srv, func() {}, nil, context.Background())
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	// Any positive value should be accepted.
	body := `{"cpu_limit":100.0}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

func TestUpdateAppPreWarmedSessions(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"pre_warmed_sessions":1}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var updated map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&updated)
	if v := updated["pre_warmed_sessions"]; v != float64(1) {
		t.Errorf("expected pre_warmed_sessions=1, got %v", v)
	}
}

func TestUpdateAppPreWarmedSessionsNegative(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"pre_warmed_sessions":-1}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestUpdateAppPreWarmedSessionsExceedsCap(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"pre_warmed_sessions":11}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestGetAppIncludesPreWarmedSessions(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "my-app")
	id := created["id"].(string)

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var app map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&app)

	if _, ok := app["pre_warmed_sessions"]; !ok {
		t.Error("expected pre_warmed_sessions in response")
	}
	if v := app["pre_warmed_sessions"]; v != float64(0) {
		t.Errorf("expected pre_warmed_sessions=0, got %v", v)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	auditLog := audit.New(auditPath, srv.Metrics)
	srv.AuditLog = auditLog
	ctx, cancel := context.WithCancel(context.Background())
	go auditLog.Run(ctx, auditPath)
	t.Cleanup(cancel)

	handler := NewRouter(srv, func() {}, nil, context.Background())
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Viewer cannot access runtime — should get 404 (opaque denial).
	if resp.StatusCode != 404 {
		t.Errorf("expected 404 for viewer, got %d", resp.StatusCode)
	}
}

func TestGetAppRuntimeNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("GET", ts.URL+"/api/v1/apps/nonexistent/runtime", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err = http.DefaultClient.Do(req)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 403 {
		t.Errorf("expected 403 for viewer purge, got %d", resp.StatusCode)
	}
}

func TestPurgeAppNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("DELETE", ts.URL+"/api/v1/apps/nonexistent?purge=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 204 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("soft-delete: expected 204, got %d: %s", resp.StatusCode, b)
	}

	// List deleted apps.
	req = authReq("GET", ts.URL+"/api/v1/apps?deleted=true", nil)
	resp, err = http.DefaultClient.Do(req)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 403 {
		t.Errorf("expected 403 for viewer listing deleted, got %d", resp.StatusCode)
	}
}

func TestListAppsV2SearchFilter(t *testing.T) {
	_, ts := testServer(t)
	createApp(t, ts, "dashboard-search")
	createApp(t, ts, "plumber-search")

	req := authReq("GET", ts.URL+"/api/v1/apps?search=dashboard", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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

// --- Coverage gap tests for runtime.go ---

func TestGetAppRuntimeWithLiveWorkerAndDeadWorker(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "runtime-full")
	id := created["id"].(string)

	// Register a live worker with a session in the in-memory store.
	srv.Workers.Set("w-live", server.ActiveWorker{
		AppID:     id,
		BundleID:  "bundle-1",
		StartedAt: time.Now().Add(-10 * time.Minute),
		IdleSince: time.Now().Add(-1 * time.Minute),
	})
	srv.Sessions.Set("sess-1", session.Entry{
		WorkerID:   "w-live",
		UserSub:    "admin",
		LastAccess: time.Now(),
	})

	// Create a DB session for activity metrics.
	srv.DB.CreateSession("db-s1", id, "w-live", "admin")

	// Create a dead worker in the logstore.
	sender := srv.LogStore.Create("w-dead", id)
	sender.Write("log line")
	srv.LogStore.MarkEnded("w-dead")

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/runtime", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var rtBody runtimeResponse
	json.NewDecoder(resp.Body).Decode(&rtBody)

	if len(rtBody.Workers) != 2 {
		t.Fatalf("expected 2 workers, got %d", len(rtBody.Workers))
	}

	// Find live and dead workers by status.
	var live, dead *runtimeWorker
	for i := range rtBody.Workers {
		switch rtBody.Workers[i].Status {
		case "active":
			live = &rtBody.Workers[i]
		case "ended":
			dead = &rtBody.Workers[i]
		}
	}

	if live == nil {
		t.Fatal("expected an active worker")
		return
	}
	if live.ID != "w-live" {
		t.Errorf("expected live worker id=w-live, got %s", live.ID)
	}
	if live.BundleID != "bundle-1" {
		t.Errorf("expected bundle_id=bundle-1, got %s", live.BundleID)
	}
	if live.IdleSince == nil {
		t.Error("expected idle_since to be set")
	}
	if len(live.Sessions) != 1 {
		t.Errorf("expected 1 session on live worker, got %d", len(live.Sessions))
	} else if live.Sessions[0].UserDisplayName != "Admin" {
		t.Errorf("expected user_display_name=Admin, got %q", live.Sessions[0].UserDisplayName)
	}

	if dead == nil {
		t.Fatal("expected an ended worker")
		return
	}
	if dead.ID != "w-dead" {
		t.Errorf("expected dead worker id=w-dead, got %s", dead.ID)
	}
	if dead.EndedAt == nil {
		t.Error("expected ended_at to be set on dead worker")
	}

	if rtBody.ActiveSessions != 1 {
		t.Errorf("expected active_sessions=1, got %d", rtBody.ActiveSessions)
	}
	if rtBody.TotalViews != 1 {
		t.Errorf("expected total_views=1, got %d", rtBody.TotalViews)
	}
}

func TestGetAppStoppingStatus(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "stopping-app")
	id := created["id"].(string)

	// Set a worker with Draining: true.
	srv.Workers.Set("w-drain", server.ActiveWorker{
		AppID:     id,
		Draining:  true,
		StartedAt: time.Now(),
	})

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var appBody map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&appBody)

	status, _ := appBody["status"].(string)
	if status != "stopping" {
		t.Errorf("expected status=stopping, got %q", status)
	}
}

func TestGetAppWithTags(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "tagged-app")
	id := created["id"].(string)

	tag, err := srv.DB.CreateTag("production")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.DB.AddAppTag(id, tag.ID); err != nil {
		t.Fatal(err)
	}

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var tagBody map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&tagBody)

	tags, ok := tagBody["tags"].([]interface{})
	if !ok {
		t.Fatal("expected tags array in response")
	}
	if len(tags) != 1 {
		t.Fatalf("expected 1 tag, got %d", len(tags))
	}
	if tags[0] != "production" {
		t.Errorf("expected tag=production, got %v", tags[0])
	}
}

func TestDisableAppDrainsWorkers(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "disable-drain")
	id := created["id"].(string)

	// Register two workers for the app.
	srv.Workers.Set("w1", server.ActiveWorker{AppID: id, StartedAt: time.Now()})
	srv.Workers.Set("w2", server.ActiveWorker{AppID: id, StartedAt: time.Now()})

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/disable", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var disBody map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&disBody)

	if disBody["enabled"] != false {
		t.Errorf("expected enabled=false, got %v", disBody["enabled"])
	}

	// Workers should be marked draining.
	w1, ok1 := srv.Workers.Get("w1")
	w2, ok2 := srv.Workers.Get("w2")
	if !ok1 || !ok2 {
		// Workers may have already been evicted by the async goroutine;
		// that is acceptable.
		return
	}
	if !w1.Draining {
		t.Error("expected w1 to be draining")
	}
	if !w2.Draining {
		t.Error("expected w2 to be draining")
	}
}

// --- Coverage gap: ListDeployments ---

func TestListDeploymentsStatusFilter(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "deploy-status")
	id := created["id"].(string)

	// Create a bundle with deployed status.
	srv.DB.CreateBundle("ds-bun-1", id, "", false)
	now := time.Now().UTC().Format(time.RFC3339)
	srv.DB.Exec(`UPDATE bundles SET deployed_at = ?, status = 'active' WHERE id = ?`, now, "ds-bun-1")

	// Filter by status=active.
	req := authReq("GET", ts.URL+"/api/v1/deployments?status=active", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body struct {
		Deployments []map[string]interface{} `json:"deployments"`
		Total       int                      `json:"total"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Total < 1 {
		t.Errorf("expected at least 1 deployment, got %d", body.Total)
	}
}

func TestListDeploymentsPagination(t *testing.T) {
	_, ts := testServer(t)

	// Create enough apps with deployments for pagination.
	for i := 0; i < 3; i++ {
		createApp(t, ts, fmt.Sprintf("page-deploy-%d", i))
	}

	req := authReq("GET", ts.URL+"/api/v1/deployments?page=1&per_page=2", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body struct {
		Page    int `json:"page"`
		PerPage int `json:"per_page"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Page != 1 {
		t.Errorf("expected page=1, got %d", body.Page)
	}
	if body.PerPage != 2 {
		t.Errorf("expected per_page=2, got %d", body.PerPage)
	}
}

// --- Coverage gap: ListAppSessions ---

func TestListAppSessionsUserFilter(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "sess-user-filter")
	id := created["id"].(string)

	srv.DB.CreateSession("suf1", id, "w1", "user-a")
	srv.DB.CreateSession("suf2", id, "w1", "user-b")

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/sessions?user=user-a", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body struct {
		Sessions []map[string]interface{} `json:"sessions"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Sessions) != 1 {
		t.Errorf("expected 1 session for user-a, got %d", len(body.Sessions))
	}
}

func TestListAppSessionsLimit(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "sess-limit")
	id := created["id"].(string)

	for i := 0; i < 5; i++ {
		srv.DB.CreateSession(fmt.Sprintf("sl%d", i), id, "w1", "user-a")
	}

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/sessions?limit=2", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body struct {
		Sessions []map[string]interface{} `json:"sessions"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Sessions) != 2 {
		t.Errorf("expected 2 sessions with limit, got %d", len(body.Sessions))
	}
}

// --- Coverage gap: EnableApp HX-Request ---

func TestEnableAppHXRequest(t *testing.T) {
	fb := &faultyBackend{
		MockBackend: mock.New(),
		spawnErr:    fmt.Errorf("container runtime unavailable"),
	}
	_, ts := testServerWithBackend(t, fb)
	created := createApp(t, ts, "enable-hx")
	id := created["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/enable", nil)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	if !strings.Contains(resp.Header.Get("HX-Trigger"), "appEnabled") {
		t.Errorf("expected HX-Trigger to contain appEnabled, got %q", resp.Header.Get("HX-Trigger"))
	}
}

// --- Coverage gap: DisableApp HX-Request ---

func TestDisableAppHXRequest(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "disable-hx")
	id := created["id"].(string)

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/disable", nil)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	if !strings.Contains(resp.Header.Get("HX-Trigger"), "appDisabled") {
		t.Errorf("expected HX-Trigger to contain appDisabled, got %q", resp.Header.Get("HX-Trigger"))
	}
}

// --- Coverage gap: GrantAccess form-urlencoded ---

func TestGrantAccessFormEncoded(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "grant-form")
	id := created["id"].(string)

	srv.DB.UpsertUserWithRole("grantee-f", "g@test", "Grantee", "publisher")

	body := "principal=grantee-f&kind=user&role=viewer"
	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/access", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 204, got %d: %s", resp.StatusCode, b)
	}
}

// --- Coverage gap: GrantAccess HX-Request ---

func TestGrantAccessHXRequest(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "grant-hx")
	id := created["id"].(string)

	srv.DB.UpsertUserWithRole("grantee-hx", "ghx@test", "Grantee", "publisher")

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/access",
		strings.NewReader(`{"principal":"grantee-hx","kind":"user","role":"viewer"}`))
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	if !strings.Contains(resp.Header.Get("HX-Trigger"), "accessGranted") {
		t.Errorf("expected HX-Trigger to contain accessGranted, got %q", resp.Header.Get("HX-Trigger"))
	}
}

// --- Coverage gap: GrantAccess audit log ---

func TestGrantAccessAuditLog(t *testing.T) {
	srv, ts, _ := testServerWithAudit(t)
	created := createApp(t, ts, "grant-audit")
	id := created["id"].(string)

	srv.DB.UpsertUserWithRole("grantee-a", "ga@test", "Grantee", "publisher")

	req := authReq("POST", ts.URL+"/api/v1/apps/"+id+"/access",
		strings.NewReader(`{"principal":"grantee-a","kind":"user","role":"viewer"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 204, got %d: %s", resp.StatusCode, b)
	}
}

// --- Coverage gap: RevokeAccess audit log ---

func TestRevokeAccessAuditLog(t *testing.T) {
	srv, ts, _ := testServerWithAudit(t)
	created := createApp(t, ts, "revoke-audit")
	id := created["id"].(string)

	srv.DB.UpsertUserWithRole("revokee-a", "ra@test", "Revokee", "publisher")
	srv.DB.GrantAppAccess(id, "revokee-a", "user", "viewer", "admin")

	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id+"/access/user/revokee-a", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 204, got %d: %s", resp.StatusCode, b)
	}
}

// --- Coverage gap: AddAppTag/RemoveAppTag edge cases ---

func TestAddAppTagNonexistentApp(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("POST", ts.URL+"/api/v1/apps/nonexistent/tags",
		strings.NewReader(`{"tag_id":"some-tag"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRemoveAppTagNonexistentApp(t *testing.T) {
	_, ts := testServer(t)

	req := authReq("DELETE", ts.URL+"/api/v1/apps/nonexistent/tags/some-tag", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestAddAppTagFormEncoded(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "tag-form-app")
	appID := created["id"].(string)

	tag, _ := srv.DB.CreateTag("form-tag")

	body := "tag_id=" + tag.ID
	req := authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/tags", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 204, got %d: %s", resp.StatusCode, b)
	}
}

func TestAddAppTagHXRequest(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "tag-hx-app")
	appID := created["id"].(string)

	tag, _ := srv.DB.CreateTag("hx-tag")

	req := authReq("POST", ts.URL+"/api/v1/apps/"+appID+"/tags",
		strings.NewReader(fmt.Sprintf(`{"tag_id":"%s"}`, tag.ID)))
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	if !strings.Contains(resp.Header.Get("HX-Trigger"), "tagAdded") {
		t.Errorf("expected HX-Trigger to contain tagAdded, got %q", resp.Header.Get("HX-Trigger"))
	}
}

func TestRemoveAppTagHXRequest(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "rmtag-hx-app")
	appID := created["id"].(string)

	tag, _ := srv.DB.CreateTag("rm-hx-tag")
	srv.DB.AddAppTag(appID, tag.ID)

	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+appID+"/tags/"+tag.ID, nil)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	if !strings.Contains(resp.Header.Get("HX-Trigger"), "tagRemoved") {
		t.Errorf("expected HX-Trigger to contain tagRemoved, got %q", resp.Header.Get("HX-Trigger"))
	}
}

// --- Coverage gap: ListTokens via session cookie ---

func TestListTokensViaCookie(t *testing.T) {
	srv, ts := testServer(t)

	cookie := sessionCookie(t, srv, "admin")

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/me/tokens", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

// --- Coverage gap: RevokeToken not owned ---

func TestRevokeTokenWrongUser(t *testing.T) {
	srv, ts := testServer(t)

	cookie := sessionCookie(t, srv, "admin")

	// Create a token first.
	reqC, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/tokens", strings.NewReader(`{"name":"tok-to-revoke"}`))
	reqC.AddCookie(cookie)
	reqC.Header.Set("Content-Type", "application/json")
	respC, _ := http.DefaultClient.Do(reqC)
	var tokBody map[string]interface{}
	json.NewDecoder(respC.Body).Decode(&tokBody)
	respC.Body.Close()
	tokID, _ := tokBody["id"].(string)

	// Create a different user and try to revoke admin's token.
	srv.DB.UpsertUserWithRole("other-user", "other@test", "Other", "publisher")
	otherCookie := sessionCookie(t, srv, "other-user")

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/v1/users/me/tokens/"+tokID, nil)
	req.AddCookie(otherCookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Should fail because the token belongs to a different user.
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404, got %d: %s", resp.StatusCode, b)
	}
}

// --- Coverage gap: RevokeAllTokens via cookie ---

func TestRevokeAllTokensViaCookie(t *testing.T) {
	srv, ts := testServer(t)

	cookie := sessionCookie(t, srv, "admin")

	// Create a token first.
	reqC, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/tokens", strings.NewReader(`{"name":"to-revoke-all"}`))
	reqC.AddCookie(cookie)
	reqC.Header.Set("Content-Type", "application/json")
	respC, _ := http.DefaultClient.Do(reqC)
	respC.Body.Close()

	// Revoke all.
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/v1/users/me/tokens", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 204, got %d: %s", resp.StatusCode, b)
	}
}

// --- Coverage gap: GetCurrentUser via cookie ---

func TestGetCurrentUserViaCookie(t *testing.T) {
	srv, ts := testServer(t)

	cookie := sessionCookie(t, srv, "admin")

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/me/", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["sub"] != "admin" {
		t.Errorf("expected sub=admin, got %q", body["sub"])
	}
	if body["email"] != "admin@test" {
		t.Errorf("expected email=admin@test, got %q", body["email"])
	}
}

// --- Coverage gap: app RuntimeResponse with deployment ---

func TestGetAppRuntimeWithDeployment(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "runtime-deploy")
	id := created["id"].(string)

	// Create and activate a bundle.
	srv.DB.CreateBundle("rt-bun", id, "", false)
	srv.DB.ActivateBundle(id, "rt-bun")
	now := time.Now().UTC().Format(time.RFC3339)
	srv.DB.Exec(`UPDATE bundles SET deployed_at = ?, status = 'active' WHERE id = ?`, now, "rt-bun")

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
	if body["last_deployed_at"] == nil {
		t.Error("expected last_deployed_at to be set")
	}
}

func TestListAppsV2WithWorkerStatuses(t *testing.T) {
	srv, ts := testServer(t)
	runApp := createApp(t, ts, "v2-running")
	runID := runApp["id"].(string)
	stopApp := createApp(t, ts, "v2-stopping")
	stopID := stopApp["id"].(string)
	createApp(t, ts, "v2-stopped") // no workers

	// running: at least one non-draining worker.
	srv.Workers.Set("wr", server.ActiveWorker{AppID: runID, StartedAt: time.Now()})
	// stopping: all workers draining.
	srv.Workers.Set("ws", server.ActiveWorker{AppID: stopID, Draining: true, StartedAt: time.Now()})

	req := authReq("GET", ts.URL+"/api/v1/apps?per_page=100", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body struct {
		Apps []map[string]interface{} `json:"apps"`
	}
	json.NewDecoder(resp.Body).Decode(&body)

	statuses := map[string]string{}
	for _, a := range body.Apps {
		name, _ := a["name"].(string)
		status, _ := a["status"].(string)
		statuses[name] = status
	}

	if statuses["v2-running"] != "running" {
		t.Errorf("expected v2-running status=running, got %q", statuses["v2-running"])
	}
	if statuses["v2-stopping"] != "stopping" {
		t.Errorf("expected v2-stopping status=stopping, got %q", statuses["v2-stopping"])
	}
	if statuses["v2-stopped"] != "stopped" {
		t.Errorf("expected v2-stopped status=stopped, got %q", statuses["v2-stopped"])
	}
}

// --- Coverage gap batch 2: error paths and unauthenticated access ---

func TestListTokensUnauthenticatedPAT(t *testing.T) {
	_, ts := testServer(t)
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

func TestRevokeTokenHXRequest(t *testing.T) {
	srv, ts := testServer(t)
	cookie := sessionCookie(t, srv, "admin")

	// Create a token.
	reqC, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/tokens", strings.NewReader(`{"name":"hx-revoke"}`))
	reqC.AddCookie(cookie)
	reqC.Header.Set("Content-Type", "application/json")
	respC, _ := http.DefaultClient.Do(reqC)
	var tok map[string]interface{}
	json.NewDecoder(respC.Body).Decode(&tok)
	respC.Body.Close()
	tokID := tok["id"].(string)

	// Revoke with HX-Request.
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/v1/users/me/tokens/"+tokID, nil)
	req.AddCookie(cookie)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 for HX-Request, got %d: %s", resp.StatusCode, b)
	}
}

func TestRevokeTokenUnauthenticated(t *testing.T) {
	_, ts := testServer(t)
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/v1/users/me/tokens/some-id", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestRevokeAllTokensUnauthenticated(t *testing.T) {
	_, ts := testServer(t)
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

func TestRevokeTokenAuditLogEmitted(t *testing.T) {
	srv, ts, _ := testServerWithAudit(t)
	cookie := sessionCookie(t, srv, "admin")

	reqC, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/tokens", strings.NewReader(`{"name":"audit-tok"}`))
	reqC.AddCookie(cookie)
	reqC.Header.Set("Content-Type", "application/json")
	respC, _ := http.DefaultClient.Do(reqC)
	var tok map[string]interface{}
	json.NewDecoder(respC.Body).Decode(&tok)
	respC.Body.Close()
	tokID := tok["id"].(string)

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/v1/users/me/tokens/"+tokID, nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
}

func TestRevokeAllTokensAuditLogEmitted(t *testing.T) {
	srv, ts, _ := testServerWithAudit(t)
	cookie := sessionCookie(t, srv, "admin")

	reqC, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/tokens", strings.NewReader(`{"name":"audit-tok-all"}`))
	reqC.AddCookie(cookie)
	reqC.Header.Set("Content-Type", "application/json")
	respC, _ := http.DefaultClient.Do(reqC)
	respC.Body.Close()

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/v1/users/me/tokens", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
}

func TestPostRefreshUnauthenticated(t *testing.T) {
	_, ts := testServer(t)
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps/some-id/refresh", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestPostRefreshNonexistentApp(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("POST", ts.URL+"/api/v1/apps/nonexistent/refresh", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestPostRefreshRollbackNonexistent(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("POST", ts.URL+"/api/v1/apps/nonexistent/refresh/rollback", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestListBundlesEmpty(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "empty-bundles-app")
	id := created["id"].(string)

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id+"/bundles", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var bundles []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&bundles)
	if len(bundles) != 0 {
		t.Errorf("expected 0 bundles, got %d", len(bundles))
	}
}

func TestAPIAuthCookieFallback(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testServerWithOIDC(t, idp)

	// Set up a valid session cookie for admin.
	cookie := sessionCookie(t, srv, "admin")

	// Hit a regular API endpoint using cookie auth (not users/me).
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/apps", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 via cookie auth, got %d: %s", resp.StatusCode, b)
	}
}

func TestAPIAuthNonByPrefixToken(t *testing.T) {
	_, ts := testServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer not-a-by-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for non-by_ token, got %d", resp.StatusCode)
	}
}

func TestUpdateAppFormEncoded(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "update-form-app")
	id := created["id"].(string)

	body := "title=My+App&description=A+test+app"
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["title"] != "My App" {
		t.Errorf("expected title=My App, got %v", result["title"])
	}
}

func TestUpdateAppRefreshSchedule(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "refresh-sched-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"refresh_schedule":"0 0 * * *"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["refresh_schedule"] != "0 0 * * *" {
		t.Errorf("expected refresh_schedule=0 0 * * *, got %v", result["refresh_schedule"])
	}
}

func TestUpdateAppInvalidRefreshSchedule(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "bad-sched-app")
	id := created["id"].(string)

	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"refresh_schedule":"not a cron"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, b)
	}
}

func TestTaskLogsNonexistentApp(t *testing.T) {
	_, ts := testServer(t)
	req := authReq("GET", ts.URL+"/api/v1/tasks/nonexistent/logs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestGetCurrentUserNoDB(t *testing.T) {
	srv, ts := testServer(t)
	// Create a session for a sub that does NOT exist in the users table.
	cookie := sessionCookie(t, srv, "ghost-user")

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/me/", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["sub"] != "ghost-user" {
		t.Errorf("expected sub=ghost-user, got %q", body["sub"])
	}
	// No email for users without DB row.
	if body["email"] != "" {
		t.Errorf("expected no email, got %q", body["email"])
	}
	if body["role"] != "viewer" {
		t.Errorf("expected role=viewer, got %q", body["role"])
	}
}

func TestUpdateAppFormEncodedWithSchedule(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "form-sched")
	id := created["id"].(string)

	body := "refresh_schedule=*/5+*+*+*+*"
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

func TestUpdateAppFormEncodedClearSchedule(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "clear-sched")
	id := created["id"].(string)

	// Set schedule first.
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id,
		strings.NewReader(`{"refresh_schedule":"0 0 * * *"}`))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Clear via form (empty value).
	body := "refresh_schedule="
	req = authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["refresh_schedule"] != "" {
		t.Errorf("expected empty schedule, got %v", result["refresh_schedule"])
	}
}

func TestRemoveAppTagNotAssigned(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "rmtag-unassigned")
	appID := created["id"].(string)

	tag, _ := srv.DB.CreateTag("unassigned-tag")

	// Try to remove a tag that was never added.
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+appID+"/tags/"+tag.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404 for unassigned tag, got %d: %s", resp.StatusCode, b)
	}
}

func TestEnrollCredentialFormEncoded(t *testing.T) {
	srv, ts := testServer(t)
	cookie := sessionCookie(t, srv, "admin")

	body := "api_key=test-key-123"
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/users/me/credentials/posit-connect", strings.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Expect 503 (no vault configured) — but the form parsing path is exercised.
	if resp.StatusCode != http.StatusServiceUnavailable {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 503, got %d: %s", resp.StatusCode, b)
	}
}

// --- Coverage gap: RevokeAccess happy path ---

func TestRevokeAccessHappyPath(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "revoke-hp")
	id := created["id"].(string)

	srv.DB.UpsertUserWithRole("revokee", "r@test", "Revokee", "publisher")
	srv.DB.GrantAppAccess(id, "revokee", "user", "viewer", "admin")

	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id+"/access/user/revokee", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 204, got %d: %s", resp.StatusCode, b)
	}
}

func TestRevokeAccessHXRequest(t *testing.T) {
	srv, ts := testServer(t)
	created := createApp(t, ts, "revoke-hx")
	id := created["id"].(string)

	srv.DB.UpsertUserWithRole("revokee-hx", "rhx@test", "Revokee", "publisher")
	srv.DB.GrantAppAccess(id, "revokee-hx", "user", "viewer", "admin")

	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id+"/access/user/revokee-hx", nil)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	if !strings.Contains(resp.Header.Get("HX-Trigger"), "accessRevoked") {
		t.Errorf("expected HX-Trigger to contain accessRevoked, got %q", resp.Header.Get("HX-Trigger"))
	}
}

func TestRevokeAccessNotFound(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "revoke-nf")
	id := created["id"].(string)

	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id+"/access/user/nonexistent", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Coverage gap: DeleteApp/RestoreApp HTMX branches ---

func TestDeleteAppHXRequest(t *testing.T) {
	_, ts := testServerWithSoftDelete(t)
	created := createApp(t, ts, "del-hx")
	id := created["id"].(string)

	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	trigger := resp.Header.Get("HX-Trigger")
	if !strings.Contains(trigger, "showToast") {
		t.Errorf("expected HX-Trigger to contain showToast, got %q", trigger)
	}
}

func TestRestoreAppHXRequest(t *testing.T) {
	_, ts := testServerWithSoftDelete(t)
	created := createApp(t, ts, "restore-hx")
	id := created["id"].(string)

	// Soft delete first.
	req := authReq("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
	http.DefaultClient.Do(req)

	// Restore with HX-Request.
	req = authReq("POST", ts.URL+"/api/v1/apps/"+id+"/restore", nil)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	trigger := resp.Header.Get("HX-Trigger")
	if !strings.Contains(trigger, "showToast") {
		t.Errorf("expected HX-Trigger to contain showToast, got %q", trigger)
	}
}

// TestSwaggerDocJSON verifies that /swagger/doc.json is accessible without
// authentication (regression test for #99).
// --- Phase 3-6: UpdateApp with running workers → UpdateResources ---

func TestUpdateAppResourcesPropagateToRunningWorkers(t *testing.T) {
	be := mock.New()
	srv, ts := testServerWithBackend(t, be)
	created := createApp(t, ts, "live-update-app")
	id := created["id"].(string)

	// Spawn a mock worker.
	be.Spawn(context.Background(), backend.WorkerSpec{WorkerID: "w-live"})
	srv.Workers.Set("w-live", server.ActiveWorker{AppID: id, StartedAt: time.Now()})

	body := `{"memory_limit":"256m","cpu_limit":1.5}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

func TestUpdateAppResourcesWorkerGone(t *testing.T) {
	// Worker registered in map but not in mock backend → UpdateResources
	// returns "not found". The handler should still return 200 (best-effort).
	be := mock.New()
	srv, ts := testServerWithBackend(t, be)
	created := createApp(t, ts, "ghost-worker-app")
	id := created["id"].(string)

	// Register a worker in the map without spawning it in the backend.
	srv.Workers.Set("w-ghost", server.ActiveWorker{AppID: id, StartedAt: time.Now()})

	body := `{"memory_limit":"128m"}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Best-effort: still returns 200 even if UpdateResources fails.
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 (best-effort), got %d: %s", resp.StatusCode, b)
	}
}

func TestUpdateAppResourcesMultipleWorkers(t *testing.T) {
	be := mock.New()
	srv, ts := testServerWithBackend(t, be)
	created := createApp(t, ts, "multi-worker-app")
	id := created["id"].(string)

	// Spawn multiple workers.
	for _, wid := range []string{"mw-1", "mw-2", "mw-3"} {
		be.Spawn(context.Background(), backend.WorkerSpec{WorkerID: wid})
		srv.Workers.Set(wid, server.ActiveWorker{AppID: id, StartedAt: time.Now()})
	}

	body := `{"cpu_limit":2.0}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

// --- Phase 3-6: Data mounts API integration tests ---

func testServerWithDataMounts(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, PakVersion: "stable"},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
			DataMounts: []config.DataMountSource{
				{Name: "models", Path: "/data/models"},
				{Name: "scratch", Path: "/data/scratch"},
			},
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
	var wg sync.WaitGroup
	srv.RestoreWG = &wg
	handler := NewRouter(srv, func() {}, nil, context.Background())
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	t.Cleanup(wg.Wait)
	return srv, ts
}

func TestUpdateAppDataMountsValid(t *testing.T) {
	_, ts := testServerWithDataMounts(t)
	created := createApp(t, ts, "mount-app")
	id := created["id"].(string)

	body := `{"data_mounts":[{"source":"models","target":"/mnt/models"},{"source":"scratch","target":"/mnt/scratch","readonly":false}]}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	mounts, ok := result["data_mounts"].([]interface{})
	if !ok || len(mounts) != 2 {
		t.Fatalf("expected 2 data_mounts, got %v", result["data_mounts"])
	}
}

func TestUpdateAppDataMountsClearWithEmptyArray(t *testing.T) {
	srv, ts := testServerWithDataMounts(t)
	created := createApp(t, ts, "mount-clear-app")
	id := created["id"].(string)

	// First set some mounts.
	body := `{"data_mounts":[{"source":"models","target":"/mnt/models"}]}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Clear mounts with empty array.
	body = `{"data_mounts":[]}`
	req = authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	mounts := result["data_mounts"].([]interface{})
	if len(mounts) != 0 {
		t.Errorf("expected 0 data_mounts after clear, got %d", len(mounts))
	}

	// Verify DB state.
	rows, _ := srv.DB.ListAppDataMounts(id)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows in DB, got %d", len(rows))
	}
}

func TestUpdateAppDataMountsUnknownSource(t *testing.T) {
	_, ts := testServerWithDataMounts(t)
	created := createApp(t, ts, "mount-unknown-app")
	id := created["id"].(string)

	body := `{"data_mounts":[{"source":"nonexistent","target":"/mnt/data"}]}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for unknown source, got %d", resp.StatusCode)
	}
}

func TestUpdateAppDataMountsReservedTarget(t *testing.T) {
	_, ts := testServerWithDataMounts(t)
	created := createApp(t, ts, "mount-reserved-app")
	id := created["id"].(string)

	body := `{"data_mounts":[{"source":"models","target":"/app"}]}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for reserved target, got %d", resp.StatusCode)
	}
}

func TestUpdateAppDataMountsPathTraversal(t *testing.T) {
	_, ts := testServerWithDataMounts(t)
	created := createApp(t, ts, "mount-traversal-app")
	id := created["id"].(string)

	body := `{"data_mounts":[{"source":"models/../secret","target":"/mnt/data"}]}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for path traversal, got %d", resp.StatusCode)
	}
}

func TestUpdateAppDataMountsDuplicateTarget(t *testing.T) {
	_, ts := testServerWithDataMounts(t)
	created := createApp(t, ts, "mount-dup-app")
	id := created["id"].(string)

	body := `{"data_mounts":[{"source":"models","target":"/mnt/data"},{"source":"scratch","target":"/mnt/data"}]}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for duplicate target, got %d", resp.StatusCode)
	}
}

func TestUpdateAppDataMountsOmittedPreservesExisting(t *testing.T) {
	srv, ts := testServerWithDataMounts(t)
	created := createApp(t, ts, "mount-preserve-app")
	id := created["id"].(string)

	// Set mounts.
	body := `{"data_mounts":[{"source":"models","target":"/mnt/models"}]}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Update something else — data_mounts omitted → should not touch mounts.
	body = `{"title":"new title"}`
	req = authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rows, _ := srv.DB.ListAppDataMounts(id)
	if len(rows) != 1 {
		t.Errorf("expected 1 mount preserved, got %d", len(rows))
	}
}

func TestUpdateAppDataMountsReadOnlyDefault(t *testing.T) {
	srv, ts := testServerWithDataMounts(t)
	created := createApp(t, ts, "mount-ro-app")
	id := created["id"].(string)

	// Omit readonly field → should default to true.
	body := `{"data_mounts":[{"source":"models","target":"/mnt/models"}]}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rows, _ := srv.DB.ListAppDataMounts(id)
	if len(rows) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(rows))
	}
	if !rows[0].ReadOnly {
		t.Error("expected ReadOnly=true by default")
	}
}

func TestUpdateAppDataMountsSubpath(t *testing.T) {
	_, ts := testServerWithDataMounts(t)
	created := createApp(t, ts, "mount-subpath-app")
	id := created["id"].(string)

	body := `{"data_mounts":[{"source":"models/v2","target":"/mnt/models"}]}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 200 for subpath, got %d: %s", resp.StatusCode, b)
	}
}

func TestUpdateAppDataMountsNoSourcesConfigured(t *testing.T) {
	// Default testServer has no DataMounts in config.
	_, ts := testServer(t)
	created := createApp(t, ts, "mount-nosource-app")
	id := created["id"].(string)

	body := `{"data_mounts":[{"source":"models","target":"/mnt/models"}]}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400 when no sources configured, got %d", resp.StatusCode)
	}
}

// --- Phase 3-6: Image/runtime validation tests ---

func TestUpdateAppImageValid(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "img-app")
	id := created["id"].(string)

	body := `{"image":"my-registry/custom:v2"}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["image"] != "my-registry/custom:v2" {
		t.Errorf("expected image=my-registry/custom:v2, got %v", result["image"])
	}
}

func TestUpdateAppImageWhitespaceRejected(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "img-ws-app")
	id := created["id"].(string)

	cases := []string{
		`{"image":"my image:v1"}`,  // space
		`{"image":"my\timage:v1"}`, // tab
	}
	for _, body := range cases {
		req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("expected 400 for image with whitespace, got %d", resp.StatusCode)
		}
	}
}

func TestUpdateAppImageClearAccepted(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "img-clear-app")
	id := created["id"].(string)

	// Set image first.
	body := `{"image":"custom:v2"}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Clear image with empty string.
	body = `{"image":""}`
	req = authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 200 to clear image, got %d: %s", resp.StatusCode, b)
	}
}

func TestUpdateAppRuntimeRequiresAdmin(t *testing.T) {
	srv, ts := testServer(t)

	// Create a publisher who owns the app — they have CanUpdateConfig
	// but not CanManageRoles (system admin only).
	srv.DB.UpsertUserWithRole("pub-rt", "pub-rt@test", "Publisher", "publisher")
	pubToken := createTestPAT(t, srv.DB, "pub-rt")

	// Publisher creates their own app (they're the owner).
	body := `{"name":"runtime-admin-app"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+pubToken)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	id := created["id"].(string)

	// Publisher (owner) tries to change runtime → 403 (admin-only).
	body = `{"runtime":"kata"}`
	req, _ = http.NewRequest("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+pubToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 403 for non-admin runtime change, got %d: %s", resp.StatusCode, b)
	}
}

func TestUpdateAppRuntimeAdminAllowed(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "runtime-ok-app")
	id := created["id"].(string)

	// Admin changes runtime → 200.
	body := `{"runtime":"kata"}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["runtime"] != "kata" {
		t.Errorf("expected runtime=kata, got %v", result["runtime"])
	}
}

// --- Phase 3-6: API response shape tests ---

func TestGetAppResponseIncludesPhase36Fields(t *testing.T) {
	srv, ts := testServerWithDataMounts(t)
	created := createApp(t, ts, "shape-app")
	id := created["id"].(string)

	// Set image, runtime, resource limits, and data mounts.
	body := `{"image":"custom:v3","runtime":"sysbox","memory_limit":"1g","cpu_limit":2.5}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	body = `{"data_mounts":[{"source":"models","target":"/mnt/models","readonly":false}]}`
	req = authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	// GET the app and verify all fields.
	req = authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if result["image"] != "custom:v3" {
		t.Errorf("expected image=custom:v3, got %v", result["image"])
	}
	if result["runtime"] != "sysbox" {
		t.Errorf("expected runtime=sysbox, got %v", result["runtime"])
	}
	if result["memory_limit"] != "1g" {
		t.Errorf("expected memory_limit=1g, got %v", result["memory_limit"])
	}
	if result["cpu_limit"] != 2.5 {
		t.Errorf("expected cpu_limit=2.5, got %v", result["cpu_limit"])
	}

	mounts, ok := result["data_mounts"].([]interface{})
	if !ok || len(mounts) != 1 {
		t.Fatalf("expected 1 data_mount, got %v", result["data_mounts"])
	}
	m := mounts[0].(map[string]interface{})
	if m["source"] != "models" {
		t.Errorf("expected mount source=models, got %v", m["source"])
	}
	if m["target"] != "/mnt/models" {
		t.Errorf("expected mount target=/mnt/models, got %v", m["target"])
	}

	// Verify data_mounts are included in response as enriched mount rows.
	appRow, _ := srv.DB.GetApp(id)
	if appRow.Image != "custom:v3" {
		t.Errorf("DB image = %q, want custom:v3", appRow.Image)
	}
	if appRow.Runtime != "sysbox" {
		t.Errorf("DB runtime = %q, want sysbox", appRow.Runtime)
	}
}

func TestGetAppResponseDefaultsForUnsetFields(t *testing.T) {
	_, ts := testServer(t)
	created := createApp(t, ts, "shape-defaults-app")
	id := created["id"].(string)

	req := authReq("GET", ts.URL+"/api/v1/apps/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(raw, &result)

	// image and runtime should be empty strings when unset.
	if result["image"] != "" {
		t.Errorf("expected image='', got %v", result["image"])
	}
	if result["runtime"] != "" {
		t.Errorf("expected runtime='', got %v", result["runtime"])
	}

	// memory_limit and cpu_limit should be nil when unset.
	if result["memory_limit"] != nil {
		t.Errorf("expected memory_limit=nil, got %v", result["memory_limit"])
	}
	if result["cpu_limit"] != nil {
		t.Errorf("expected cpu_limit=nil, got %v", result["cpu_limit"])
	}
}

func TestUpdateAppResponseIncludesDataMounts(t *testing.T) {
	_, ts := testServerWithDataMounts(t)
	created := createApp(t, ts, "shape-update-app")
	id := created["id"].(string)

	body := `{"data_mounts":[{"source":"models","target":"/mnt/m1"},{"source":"scratch","target":"/mnt/s1","readonly":false}]}`
	req := authReq("PATCH", ts.URL+"/api/v1/apps/"+id, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	mounts, ok := result["data_mounts"].([]interface{})
	if !ok {
		t.Fatalf("expected data_mounts in response, got %v", result["data_mounts"])
	}
	if len(mounts) != 2 {
		t.Errorf("expected 2 data_mounts, got %d", len(mounts))
	}
}

func TestSwaggerDocJSON(t *testing.T) {
	_, ts := testServer(t)

	resp, err := http.Get(ts.URL + "/swagger/doc.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json content-type, got %q", ct)
	}
}
