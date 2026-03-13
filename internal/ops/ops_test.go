package ops

import (
	"context"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
)

func testServer(t *testing.T) (*server.Server, *mock.MockBackend) {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{Token: config.NewSecret("test-token")},
		Docker: config.DockerConfig{Image: "test-image", ShinyPort: 3838},
		Storage: config.StorageConfig{
			BundleServerPath: t.TempDir(),
			BundleWorkerPath: "/app",
		},
		Proxy: config.ProxyConfig{
			MaxWorkers:     100,
			HealthInterval: config.Duration{Duration: 50 * time.Millisecond},
			LogRetention:   config.Duration{Duration: 1 * time.Hour},
		},
	}
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	return srv, be
}

func spawnWorker(t *testing.T, srv *server.Server, be *mock.MockBackend, workerID, appID string) {
	t.Helper()
	err := be.Spawn(context.Background(), backend.WorkerSpec{
		WorkerID: workerID,
		AppID:    appID,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Workers.Set(workerID, server.ActiveWorker{AppID: appID})
	srv.Registry.Set(workerID, "127.0.0.1:9999")
}

func TestEvictWorker(t *testing.T) {
	srv, be := testServer(t)
	spawnWorker(t, srv, be, "w1", "app1")
	srv.Sessions.Set("sess1", session.Entry{WorkerID: "w1"})
	srv.LogStore.Create("w1", "app1")

	EvictWorker(context.Background(), srv, "w1")

	if _, ok := srv.Workers.Get("w1"); ok {
		t.Error("worker should be removed from WorkerMap")
	}
	if _, ok := srv.Registry.Get("w1"); ok {
		t.Error("worker should be removed from Registry")
	}
	if _, ok := srv.Sessions.Get("sess1"); ok {
		t.Error("session should be deleted")
	}
	if be.HasWorker("w1") {
		t.Error("backend should have stopped the worker")
	}
	if srv.LogStore.HasActive("w1") {
		t.Error("log should be marked ended")
	}
}

func TestEvictWorkerIdempotent(t *testing.T) {
	srv, be := testServer(t)
	spawnWorker(t, srv, be, "w1", "app1")

	EvictWorker(context.Background(), srv, "w1")
	EvictWorker(context.Background(), srv, "w1") // must not panic
}

func TestStartupCleanupRemovesOrphans(t *testing.T) {
	srv, be := testServer(t)

	be.SetManagedResources([]backend.ManagedResource{
		{ID: "container-1", Kind: backend.ResourceContainer},
		{ID: "network-1", Kind: backend.ResourceNetwork},
	})

	if err := StartupCleanup(context.Background(), srv); err != nil {
		t.Fatal(err)
	}

	resources, _ := be.ListManaged(context.Background())
	if len(resources) != 0 {
		t.Errorf("expected 0 managed resources after cleanup, got %d", len(resources))
	}
}

func TestStartupCleanupFailsStaleBuilds(t *testing.T) {
	srv, _ := testServer(t)

	app, err := srv.DB.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := srv.DB.CreateBundle("bundle-1", app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.DB.UpdateBundleStatus(bundle.ID, "building"); err != nil {
		t.Fatal(err)
	}

	if err := StartupCleanup(context.Background(), srv); err != nil {
		t.Fatal(err)
	}

	b, err := srv.DB.GetBundle(bundle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "failed" {
		t.Errorf("expected bundle status 'failed', got %q", b.Status)
	}
}

func TestHealthPollerEvictsAfterConsecutiveMisses(t *testing.T) {
	srv, be := testServer(t)
	spawnWorker(t, srv, be, "w1", "app1")

	be.HealthOK.Store(false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	misses := make(map[string]int)

	// First poll — miss count = 1, no eviction
	pollOnce(ctx, srv, misses)
	if _, ok := srv.Workers.Get("w1"); !ok {
		t.Fatal("worker should survive first miss")
	}

	// Second poll — miss count = 2, eviction
	pollOnce(ctx, srv, misses)
	if _, ok := srv.Workers.Get("w1"); ok {
		t.Error("worker should be evicted after 2 consecutive misses")
	}
}

func TestHealthPollerResetsOnRecovery(t *testing.T) {
	srv, be := testServer(t)
	spawnWorker(t, srv, be, "w1", "app1")

	misses := make(map[string]int)
	ctx := context.Background()

	// Miss once
	be.HealthOK.Store(false)
	pollOnce(ctx, srv, misses)
	if misses["w1"] != 1 {
		t.Fatalf("expected 1 miss, got %d", misses["w1"])
	}

	// Recover
	be.HealthOK.Store(true)
	pollOnce(ctx, srv, misses)
	if _, exists := misses["w1"]; exists {
		t.Error("miss count should be cleared after recovery")
	}

	// Miss again — should not evict (counter reset)
	be.HealthOK.Store(false)
	pollOnce(ctx, srv, misses)
	if _, ok := srv.Workers.Get("w1"); !ok {
		t.Error("worker should not be evicted after a single miss post-recovery")
	}
}

func TestHealthPollerKeepsHealthyWorkers(t *testing.T) {
	srv, be := testServer(t)
	spawnWorker(t, srv, be, "w1", "app1")

	misses := make(map[string]int)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		pollOnce(ctx, srv, misses)
	}

	if _, ok := srv.Workers.Get("w1"); !ok {
		t.Error("healthy worker should not be evicted")
	}
}

func TestLogCaptureStoresWorkerLogs(t *testing.T) {
	srv, be := testServer(t)
	be.SetLogLines([]string{"line 1", "line 2", "line 3"})

	SpawnLogCapture(context.Background(), srv, "w1", "app1")

	// Give the goroutine time to process
	time.Sleep(100 * time.Millisecond)

	snapshot, _, ok := srv.LogStore.Subscribe("w1")
	if !ok {
		t.Fatal("expected log entry to exist")
	}
	if len(snapshot) != 3 {
		t.Errorf("expected 3 log lines, got %d", len(snapshot))
	}
}

func TestLogCaptureMarksEndedWhenStreamCloses(t *testing.T) {
	srv, be := testServer(t)
	be.SetLogLines([]string{"line 1"})

	SpawnLogCapture(context.Background(), srv, "w1", "app1")

	// Give the goroutine time to finish
	time.Sleep(100 * time.Millisecond)

	if srv.LogStore.HasActive("w1") {
		t.Error("log should be marked ended after stream closes")
	}
}

func TestGracefulShutdownStopsAllWorkers(t *testing.T) {
	srv, be := testServer(t)
	spawnWorker(t, srv, be, "w1", "app1")
	spawnWorker(t, srv, be, "w2", "app2")

	GracefulShutdown(context.Background(), srv)

	if len(srv.Workers.All()) != 0 {
		t.Error("expected all workers removed after shutdown")
	}
	if be.WorkerCount() != 0 {
		t.Error("expected backend to have no workers after shutdown")
	}
}

func TestGracefulShutdownFailsInProgressBuilds(t *testing.T) {
	srv, _ := testServer(t)

	app, err := srv.DB.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := srv.DB.CreateBundle("bundle-1", app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.DB.UpdateBundleStatus(bundle.ID, "building"); err != nil {
		t.Fatal(err)
	}

	GracefulShutdown(context.Background(), srv)

	b, err := srv.DB.GetBundle(bundle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "failed" {
		t.Errorf("expected bundle status 'failed', got %q", b.Status)
	}
}

func TestLogRetentionCleaner(t *testing.T) {
	srv, _ := testServer(t)
	srv.Config.Proxy.LogRetention.Duration = 50 * time.Millisecond

	sender := srv.LogStore.Create("w1", "app1")
	sender.Write("line 1")
	srv.LogStore.MarkEnded("w1")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		SpawnLogRetentionCleaner(ctx, srv)
		close(done)
	}()

	// Wait for retention to expire and cleaner to run
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	_, _, ok := srv.LogStore.Subscribe("w1")
	if ok {
		t.Error("expected expired log entry to be cleaned up")
	}
}

func TestSpawnHealthPollerStopsOnCancel(t *testing.T) {
	srv, _ := testServer(t)
	srv.Config.Proxy.HealthInterval = config.Duration{Duration: 50 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		SpawnHealthPoller(ctx, srv)
		close(done)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// SpawnHealthPoller returned — success.
	case <-time.After(2 * time.Second):
		t.Fatal("SpawnHealthPoller did not return after context cancel")
	}
}

func TestPollOncePrunesRemovedWorkers(t *testing.T) {
	srv, be := testServer(t)
	spawnWorker(t, srv, be, "w1", "app1")

	misses := make(map[string]int)
	ctx := context.Background()

	// Record a miss for w1.
	be.HealthOK.Store(false)
	pollOnce(ctx, srv, misses)
	if misses["w1"] != 1 {
		t.Fatalf("expected 1 miss for w1, got %d", misses["w1"])
	}

	// Add a stale entry for a worker that no longer exists.
	misses["ghost-worker"] = 1

	// Make w1 healthy again so it doesn't get evicted.
	be.HealthOK.Store(true)
	pollOnce(ctx, srv, misses)

	// The ghost entry should be pruned.
	if _, exists := misses["ghost-worker"]; exists {
		t.Error("expected stale miss entry for ghost-worker to be pruned")
	}
}

func TestDrainAndEvictAllWithActiveSessions(t *testing.T) {
	srv, be := testServer(t)
	srv.Config.Server.ShutdownTimeout = config.Duration{Duration: 100 * time.Millisecond}

	spawnWorker(t, srv, be, "w1", "app1")
	spawnWorker(t, srv, be, "w2", "app1")
	srv.Sessions.Set("sess1", session.Entry{WorkerID: "w1"})
	srv.Sessions.Set("sess2", session.Entry{WorkerID: "w2"})

	drainAndEvictAll(context.Background(), srv, []string{"w1", "w2"})

	if len(srv.Workers.All()) != 0 {
		t.Errorf("expected all workers evicted, got %d", len(srv.Workers.All()))
	}
	if be.WorkerCount() != 0 {
		t.Errorf("expected backend to have 0 workers, got %d", be.WorkerCount())
	}
}

func TestGracefulShutdownNoWorkers(t *testing.T) {
	srv, _ := testServer(t)

	// Should not panic with no workers.
	GracefulShutdown(context.Background(), srv)

	if len(srv.Workers.All()) != 0 {
		t.Error("expected no workers")
	}
}

func TestLogCaptureHandlesStreamError(t *testing.T) {
	srv, _ := testServer(t)

	// Don't spawn any backend worker — Logs will still return a stream
	// because mock.Logs doesn't check worker existence. Instead, set
	// empty log lines so the stream closes immediately.
	// The log should be created and marked ended.
	SpawnLogCapture(context.Background(), srv, "no-worker", "app1")

	time.Sleep(100 * time.Millisecond)

	if srv.LogStore.HasActive("no-worker") {
		t.Error("log should be marked ended after stream closes")
	}
}
