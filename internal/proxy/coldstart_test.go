package proxy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

func testColdstartServer(t *testing.T) *server.Server {
	t.Helper()
	tmp := t.TempDir()

	cfg := &config.Config{
		Server: config.ServerConfig{Token: "test-token"},
		Docker: config.DockerConfig{
			Image:        "test-image",
			ShinyPort:    3838,
			RvBinaryPath: "/dummy/rv",
		},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{
			WsCacheTTL:         config.Duration{Duration: 5 * time.Second},
			WorkerStartTimeout: config.Duration{Duration: 5 * time.Second},
			MaxWorkers:         10,
		},
	}

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	be := mock.New()
	return server.NewServer(cfg, be, database)
}

func createTestApp(t *testing.T, srv *server.Server, name string, withBundle bool) *db.AppRow {
	t.Helper()
	app, err := srv.DB.CreateApp(name)
	if err != nil {
		t.Fatal(err)
	}
	if withBundle {
		_, err := srv.DB.CreateBundle("bundle-1", app.ID)
		if err != nil {
			t.Fatal(err)
		}
		srv.DB.UpdateBundleStatus("bundle-1", "ready")
		srv.DB.SetActiveBundle(app.ID, "bundle-1")
		// Re-fetch to get active_bundle
		app, err = srv.DB.GetApp(app.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	return app
}

func TestEnsureWorkerSpawnsNew(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	wid, addr, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	if wid == "" {
		t.Error("expected non-empty worker ID")
	}
	if addr == "" {
		t.Error("expected non-empty address")
	}
	if srv.Workers.Count() != 1 {
		t.Errorf("expected 1 worker, got %d", srv.Workers.Count())
	}
}

func TestEnsureWorkerReusesExisting(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Spawn first worker
	wid1, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	// Call again — should reuse
	wid2, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	if wid2 != wid1 {
		t.Errorf("expected reuse of worker %s, got %s", wid1, wid2)
	}
	if srv.Workers.Count() != 1 {
		t.Errorf("expected 1 worker, got %d", srv.Workers.Count())
	}
}

func TestEnsureWorkerMaxWorkersRejects(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Fill workers to max
	for i := range srv.Config.Proxy.MaxWorkers {
		srv.Workers.Set(
			fmt.Sprintf("fake-%d", i),
			server.ActiveWorker{AppID: "other"},
		)
	}

	_, _, err := ensureWorker(context.Background(), srv, app)
	if err != errMaxWorkers {
		t.Errorf("expected errMaxWorkers, got %v", err)
	}
}

func TestEnsureWorkerNoBundleRejects(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", false)

	_, _, err := ensureWorker(context.Background(), srv, app)
	if err != errNoBundle {
		t.Errorf("expected errNoBundle, got %v", err)
	}
}

func TestPollHealthySucceeds(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Spawn a worker so HealthCheck can find it
	wid, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	// pollHealthy on an already-healthy worker should return immediately
	if err := pollHealthy(context.Background(), srv, wid); err != nil {
		t.Errorf("expected healthy, got %v", err)
	}
}

func TestPollHealthyTimeout(t *testing.T) {
	srv := testColdstartServer(t)
	srv.Config.Proxy.WorkerStartTimeout = config.Duration{Duration: 200 * time.Millisecond}

	be := srv.Backend.(*mock.MockBackend)
	be.HealthOK.Store(false)

	// Spawn a mock worker so HealthCheck doesn't 404
	be.Spawn(context.Background(), backend.WorkerSpec{WorkerID: "timeout-worker"})
	srv.Workers.Set("timeout-worker", server.ActiveWorker{AppID: "test"})

	err := pollHealthy(context.Background(), srv, "timeout-worker")
	if err != errHealthTimeout {
		t.Errorf("expected errHealthTimeout, got %v", err)
	}
}
