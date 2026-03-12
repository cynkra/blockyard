package api

import (
	"bytes"
	"encoding/json"
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
		Server:  config.ServerConfig{Token: "test-token"},
		Docker:  config.DockerConfig{Image: "test-image", ShinyPort: 3838},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024, // 10 MiB for tests
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
	srv, ts := testServer(t)
	app, _ := srv.DB.CreateApp("test-app")

	resp, err := http.Post(
		ts.URL+"/api/v1/apps/"+app.ID+"/bundles",
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
	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/nonexistent/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer test-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestUploadBundleReturns202(t *testing.T) {
	srv, ts := testServer(t)
	app, _ := srv.DB.CreateApp("test-app")

	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+app.ID+"/bundles",
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
	srv, ts := testServer(t)
	app, _ := srv.DB.CreateApp("test-app")

	// Upload a bundle
	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+app.ID+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer test-token")
	resp, _ := http.DefaultClient.Do(req)
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	taskID := body["task_id"]

	// Give the background goroutine a moment to run
	time.Sleep(100 * time.Millisecond)

	// Fetch task logs
	req, _ = http.NewRequest("GET",
		ts.URL+"/api/v1/tasks/"+taskID+"/logs", nil)
	req.Header.Set("Authorization", "Bearer test-token")
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
	srv, ts := testServer(t)
	app, _ := srv.DB.CreateApp("test-app")

	// Upload two bundles
	for range 2 {
		req, _ := http.NewRequest("POST",
			ts.URL+"/api/v1/apps/"+app.ID+"/bundles",
			bytes.NewReader(testutil.MakeBundle(t)))
		req.Header.Set("Authorization", "Bearer test-token")
		http.DefaultClient.Do(req)
	}

	// Give restore goroutines time to finish
	time.Sleep(100 * time.Millisecond)

	req, _ := http.NewRequest("GET",
		ts.URL+"/api/v1/apps/"+app.ID+"/bundles", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, _ := http.DefaultClient.Do(req)

	var bundles []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&bundles)
	if len(bundles) != 2 {
		t.Errorf("expected 2 bundles, got %d", len(bundles))
	}
}
