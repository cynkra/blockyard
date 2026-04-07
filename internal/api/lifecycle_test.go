package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

// testServerWithLifecycle returns a server with start/stop/list routes mounted.
// These handlers are not yet wired into the main router; they need dedicated
// testing via a custom chi router.
func testServerWithLifecycle(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	return testServerWithLifecycleBackend(t, mock.New())
}

func testServerWithLifecycleBackend(t *testing.T, be backend.Backend) (*server.Server, *httptest.Server) {
	t.Helper()
	tmp := t.TempDir()

	cfg := &config.Config{
		Server: config.ServerConfig{
			ShutdownTimeout: config.Duration{Duration: 2 * time.Second},
		},
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838, PakVersion: "stable"},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{
			MaxWorkers:         100,
			WorkerStartTimeout: config.Duration{Duration: 2 * time.Second},
		},
	}

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)

	srv := server.NewServer(cfg, be, database)
	var wg sync.WaitGroup
	srv.RestoreWG = &wg

	// Build a chi router with lifecycle endpoints + the standard API routes.
	r := chi.NewRouter()
	r.Use(APIAuth(srv))
	r.Use(limitBody)
	r.Post("/apps", CreateApp(srv))
	r.Get("/apps", ListApps(srv))
	r.Get("/apps/{id}", GetApp(srv))
	r.Post("/apps/{id}/bundles", UploadBundle(srv))
	r.Post("/apps/{id}/start", StartApp(srv))
	r.Post("/apps/{id}/stop", StopApp(srv))

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	t.Cleanup(wg.Wait)

	return srv, ts
}

// createLifecycleApp creates an app and uploads a bundle so the app has
// an active_bundle (required for StartApp).
func createLifecycleApp(t *testing.T, ts *httptest.Server, name string) string {
	t.Helper()
	body := fmt.Sprintf(`{"name":"%s"}`, name)
	req := authReq("POST", ts.URL+"/apps", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create app %q: expected 201, got %d: %s", name, resp.StatusCode, b)
	}
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result["id"].(string)
}

// --- StartApp tests ---

func TestStartAppNoBundle(t *testing.T) {
	_, ts := testServerWithLifecycle(t)
	appID := createLifecycleApp(t, ts, "no-bundle-app")

	req := authReq("POST", ts.URL+"/apps/"+appID+"/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, b)
	}
}

func TestStartAppAlreadyRunning(t *testing.T) {
	srv, ts := testServerWithLifecycle(t)
	appID := createLifecycleApp(t, ts, "running-app")

	// Register a running worker.
	srv.Workers.Set("existing-worker", server.ActiveWorker{
		AppID: appID, StartedAt: time.Now(),
	})

	req := authReq("POST", ts.URL+"/apps/"+appID+"/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body startResponse
	json.NewDecoder(resp.Body).Decode(&body)
	if body.WorkerID != "existing-worker" {
		t.Errorf("expected worker_id=existing-worker, got %s", body.WorkerID)
	}
	if body.Status != "running" {
		t.Errorf("expected status=running, got %s", body.Status)
	}
}

func TestStartAppMaxWorkersReached(t *testing.T) {
	srv, ts := testServerWithLifecycleBackend(t, mock.New())
	appID := createLifecycleApp(t, ts, "max-workers-app")

	// Set active bundle on the app.
	srv.DB.CreateBundle("bun1", appID, "", false)
	srv.DB.ActivateBundle(appID, "bun1")

	// Fill up to max workers.
	srv.Config.Proxy.MaxWorkers = 1
	srv.Workers.Set("other-worker", server.ActiveWorker{
		AppID: "other-app", StartedAt: time.Now(),
	})

	req := authReq("POST", ts.URL+"/apps/"+appID+"/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 503, got %d: %s", resp.StatusCode, b)
	}
}

func TestStartAppSuccess(t *testing.T) {
	be := mock.New()
	srv, ts := testServerWithLifecycleBackend(t, be)
	appID := createLifecycleApp(t, ts, "start-success")

	// Set active bundle.
	srv.DB.CreateBundle("bun-start", appID, "", false)
	srv.DB.ActivateBundle(appID, "bun-start")

	req := authReq("POST", ts.URL+"/apps/"+appID+"/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body startResponse
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Status != "running" {
		t.Errorf("expected status=running, got %s", body.Status)
	}
	if body.WorkerID == "" {
		t.Error("expected non-empty worker_id")
	}

	// Worker should be in the backend.
	if be.WorkerCount() != 1 {
		t.Errorf("expected 1 worker in backend, got %d", be.WorkerCount())
	}

	// Worker should be in the worker map.
	if srv.Workers.Count() != 1 {
		t.Errorf("expected 1 worker in map, got %d", srv.Workers.Count())
	}
}

func TestStartAppSpawnFailure(t *testing.T) {
	fb := &faultyBackend{
		MockBackend: mock.New(),
		spawnErr:    fmt.Errorf("docker daemon unavailable"),
	}
	srv, ts := testServerWithLifecycleBackend(t, fb)
	appID := createLifecycleApp(t, ts, "spawn-fail")

	srv.DB.CreateBundle("bun-fail", appID, "", false)
	srv.DB.ActivateBundle(appID, "bun-fail")

	req := authReq("POST", ts.URL+"/apps/"+appID+"/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 500, got %d: %s", resp.StatusCode, b)
	}
}

func TestStartAppHealthTimeout(t *testing.T) {
	be := mock.New()
	be.HealthOK.Store(false) // worker never becomes healthy
	srv, ts := testServerWithLifecycleBackend(t, be)

	// Use a very short timeout.
	srv.Config.Proxy.WorkerStartTimeout = config.Duration{Duration: 200 * time.Millisecond}

	appID := createLifecycleApp(t, ts, "health-timeout")
	srv.DB.CreateBundle("bun-ht", appID, "", false)
	srv.DB.ActivateBundle(appID, "bun-ht")

	req := authReq("POST", ts.URL+"/apps/"+appID+"/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 503, got %d: %s", resp.StatusCode, b)
	}

	// Worker should be cleaned up from the map.
	if srv.Workers.Count() != 0 {
		t.Errorf("expected 0 workers after health timeout, got %d", srv.Workers.Count())
	}
}

func TestStartAppNonexistent(t *testing.T) {
	_, ts := testServerWithLifecycle(t)

	req := authReq("POST", ts.URL+"/apps/nonexistent-id/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- StopApp tests ---

func TestStopAppNoWorkers(t *testing.T) {
	_, ts := testServerWithLifecycle(t)
	appID := createLifecycleApp(t, ts, "stop-no-workers")

	req := authReq("POST", ts.URL+"/apps/"+appID+"/stop", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if int(body["stopped_workers"].(float64)) != 0 {
		t.Errorf("expected stopped_workers=0, got %v (body: %v)", body["stopped_workers"], body)
	}
}

func TestStopAppWithWorkers(t *testing.T) {
	srv, ts := testServerWithLifecycle(t)
	appID := createLifecycleApp(t, ts, "stop-workers")

	// Register workers for this app.
	srv.Workers.Set("sw1", server.ActiveWorker{AppID: appID, StartedAt: time.Now()})
	srv.Workers.Set("sw2", server.ActiveWorker{AppID: appID, StartedAt: time.Now()})

	req := authReq("POST", ts.URL+"/apps/"+appID+"/stop", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, raw)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v (raw: %s)", err, raw)
	}

	sw, ok := body["worker_count"].(float64)
	if !ok || int(sw) != 2 {
		t.Errorf("expected worker_count=2, got %v (body: %s)", body["worker_count"], raw)
	}
	tid, _ := body["task_id"].(string)
	if tid == "" {
		t.Error("expected non-empty task_id")
	}

	// Workers should be marked draining (or already evicted by background goroutine).
	w1, ok := srv.Workers.Get("sw1")
	if ok && !w1.Draining {
		t.Error("expected sw1 to be draining")
	}
}

func TestStopAppNonexistent(t *testing.T) {
	_, ts := testServerWithLifecycle(t)

	req := authReq("POST", ts.URL+"/apps/nonexistent-id/stop", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- ListApps (v1) tests ---

func TestListAppsV1Admin(t *testing.T) {
	_, ts := testServerWithLifecycle(t)
	createLifecycleApp(t, ts, "list-app-1")
	createLifecycleApp(t, ts, "list-app-2")

	req := authReq("GET", ts.URL+"/apps", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var apps []AppResponse
	json.NewDecoder(resp.Body).Decode(&apps)
	if len(apps) != 2 {
		t.Errorf("expected 2 apps, got %d", len(apps))
	}
}

func TestListAppsV1Publisher(t *testing.T) {
	srv, ts := testServerWithLifecycle(t)

	// Create apps owned by admin.
	srv.DB.UpsertUserWithRole("pub1", "pub1@test", "Publisher", "publisher")
	pubToken := createTestPAT(t, srv.DB, "pub1")

	createLifecycleApp(t, ts, "admin-app-1")
	createLifecycleApp(t, ts, "admin-app-2")

	// Publisher should see only accessible apps (none owned, none granted).
	req, _ := http.NewRequest("GET", ts.URL+"/apps", nil)
	req.Header.Set("Authorization", "Bearer "+pubToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	var apps []AppResponse
	json.NewDecoder(resp.Body).Decode(&apps)
	if len(apps) != 0 {
		t.Errorf("expected 0 apps for publisher, got %d", len(apps))
	}
}

func TestListAppsV1DeletedAdminOnly(t *testing.T) {
	srv, ts := testServerWithLifecycle(t)
	appID := createLifecycleApp(t, ts, "soft-del-app")

	// Soft-delete the app.
	srv.DB.SoftDeleteApp(appID)

	// Admin can see deleted apps.
	req := authReq("GET", ts.URL+"/apps?deleted=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var apps []AppResponse
	json.NewDecoder(resp.Body).Decode(&apps)
	if len(apps) != 1 {
		t.Errorf("expected 1 deleted app, got %d", len(apps))
	}
}

// --- Pure function tests ---

func TestIsValidCron(t *testing.T) {
	tests := []struct {
		expr string
		want bool
	}{
		{"* * * * *", true},
		{"0 0 * * *", true},
		{"*/5 * * * *", true},
		{"0 0 1 1 *", true},
		{"invalid", false},
		{"* * *", false},
		{"* * * * * *", false}, // 6 fields (seconds-based, not standard)
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			got := isValidCron(tc.expr)
			if got != tc.want {
				t.Errorf("isValidCron(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

func TestParseUpdateAppForm(t *testing.T) {
	form := url.Values{
		"title":                  {"My App"},
		"description":            {"A test app"},
		"access_type":            {"acl"},
		"refresh_schedule":       {"0 0 * * *"},
		"memory_limit":           {"512Mi"},
		"max_workers_per_app":    {"3"},
		"max_sessions_per_worker": {"10"},
		"pre_warmed_sessions":    {"2"},
		"cpu_limit":              {"1.5"},
	}

	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ParseForm()

	body := parseUpdateAppForm(req)

	if body.Title == nil || *body.Title != "My App" {
		t.Errorf("expected title=My App, got %v", body.Title)
	}
	if body.Description == nil || *body.Description != "A test app" {
		t.Errorf("expected description=A test app, got %v", body.Description)
	}
	if body.AccessType == nil || *body.AccessType != "acl" {
		t.Errorf("expected access_type=acl, got %v", body.AccessType)
	}
	if body.RefreshSchedule == nil || *body.RefreshSchedule != "0 0 * * *" {
		t.Errorf("expected refresh_schedule=0 0 * * *, got %v", body.RefreshSchedule)
	}
	if body.MemoryLimit == nil || *body.MemoryLimit != "512Mi" {
		t.Errorf("expected memory_limit=512Mi, got %v", body.MemoryLimit)
	}
	if body.MaxWorkersPerApp == nil || *body.MaxWorkersPerApp != 3 {
		t.Errorf("expected max_workers_per_app=3, got %v", body.MaxWorkersPerApp)
	}
	if body.MaxSessionsPerWorker == nil || *body.MaxSessionsPerWorker != 10 {
		t.Errorf("expected max_sessions_per_worker=10, got %v", body.MaxSessionsPerWorker)
	}
	if body.PreWarmedSessions == nil || *body.PreWarmedSessions != 2 {
		t.Errorf("expected pre_warmed_sessions=2, got %v", body.PreWarmedSessions)
	}
	if body.CPULimit == nil || *body.CPULimit != 1.5 {
		t.Errorf("expected cpu_limit=1.5, got %v", body.CPULimit)
	}
}

func TestParseUpdateAppFormEmptyValues(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ParseForm()

	body := parseUpdateAppForm(req)

	if body.Title != nil {
		t.Error("expected Title to be nil for empty form")
	}
	if body.MaxWorkersPerApp != nil {
		t.Error("expected MaxWorkersPerApp to be nil for empty form")
	}
	if body.CPULimit != nil {
		t.Error("expected CPULimit to be nil for empty form")
	}
}

func TestParseUpdateAppFormInvalidNumbers(t *testing.T) {
	form := url.Values{
		"max_workers_per_app":    {"not-a-number"},
		"max_sessions_per_worker": {"abc"},
		"pre_warmed_sessions":    {"xyz"},
		"cpu_limit":              {"nope"},
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ParseForm()

	body := parseUpdateAppForm(req)

	if body.MaxWorkersPerApp != nil {
		t.Error("expected MaxWorkersPerApp to be nil for invalid number")
	}
	if body.MaxSessionsPerWorker != nil {
		t.Error("expected MaxSessionsPerWorker to be nil for invalid number")
	}
	if body.PreWarmedSessions != nil {
		t.Error("expected PreWarmedSessions to be nil for invalid number")
	}
	if body.CPULimit != nil {
		t.Error("expected CPULimit to be nil for invalid number")
	}
}

func TestParseUpdateAppFormRefreshScheduleEmpty(t *testing.T) {
	// Test that refresh_schedule can be set to an empty string (clear schedule).
	form := url.Values{
		"refresh_schedule": {""},
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ParseForm()

	body := parseUpdateAppForm(req)

	if body.RefreshSchedule == nil {
		t.Fatal("expected refresh_schedule to be set (even if empty)")
	}
	if *body.RefreshSchedule != "" {
		t.Errorf("expected empty refresh_schedule, got %q", *body.RefreshSchedule)
	}
}

func TestStringOrNilNil(t *testing.T) {
	got := stringOrNil(nil)
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestStringOrNilValue(t *testing.T) {
	v := "hello"
	got := stringOrNil(&v)
	if got != "hello" {
		t.Errorf("expected hello, got %v", got)
	}
}

// --- pollWorkerHealthy tests ---

func TestPollWorkerHealthyImmediate(t *testing.T) {
	be := mock.New()
	cfg := &config.Config{}
	database, _ := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	t.Cleanup(func() { database.Close() })
	srv := server.NewServer(cfg, be, database)

	// Spawn a worker so the mock has it registered.
	be.Spawn(context.Background(), backend.WorkerSpec{WorkerID: "pw-1"})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := pollWorkerHealthy(ctx, srv, "pw-1"); err != nil {
		t.Fatalf("expected immediate success, got: %v", err)
	}
}

func TestPollWorkerHealthyTimeout(t *testing.T) {
	be := mock.New()
	be.HealthOK.Store(false)
	cfg := &config.Config{}
	database, _ := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	t.Cleanup(func() { database.Close() })
	srv := server.NewServer(cfg, be, database)

	be.Spawn(context.Background(), backend.WorkerSpec{WorkerID: "pw-2"})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err := pollWorkerHealthy(ctx, srv, "pw-2")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "did not become healthy") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- StartApp with audit logging ---

func TestStartAppAuditLog(t *testing.T) {
	be := mock.New()
	srv, ts := testServerWithLifecycleBackend(t, be)
	appID := createLifecycleApp(t, ts, "audit-start")

	srv.DB.CreateBundle("bun-audit", appID, "", false)
	srv.DB.ActivateBundle(appID, "bun-audit")

	// Enable audit logging.
	srv.AuditLog = audit.New(t.TempDir() + "/audit.log")

	req := authReq("POST", ts.URL+"/apps/"+appID+"/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
}

// --- StopApp with audit logging ---

func TestStopAppAuditLog(t *testing.T) {
	srv, ts := testServerWithLifecycle(t)
	appID := createLifecycleApp(t, ts, "audit-stop")

	srv.Workers.Set("audit-w1", server.ActiveWorker{AppID: appID, StartedAt: time.Now()})

	srv.AuditLog = audit.New(t.TempDir() + "/audit.log")

	req := authReq("POST", ts.URL+"/apps/"+appID+"/stop", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, b)
	}
}

