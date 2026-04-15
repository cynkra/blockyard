package ops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/pkgstore"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
	"github.com/cynkra/blockyard/internal/telemetry"
)

func testServer(t *testing.T) (*server.Server, *mock.MockBackend) {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{},
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
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
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

	EvictWorker(context.Background(), srv, "w1", telemetry.ReasonGraceful)

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

	EvictWorker(context.Background(), srv, "w1", telemetry.ReasonGraceful)
	EvictWorker(context.Background(), srv, "w1", telemetry.ReasonCrashed) // must not panic
}

func TestEvictWorkerCleansUpPkgStore(t *testing.T) {
	srv, be := testServer(t)
	spawnWorker(t, srv, be, "w1", "app1")

	// Set up a PkgStore with a worker library directory.
	store := pkgstore.NewStore(filepath.Join(srv.Config.Storage.BundleServerPath, ".pkg-store"))
	store.SetPlatform("4.5-x86_64-pc-linux-gnu")
	srv.PkgStore = store

	workerLib := store.WorkerLibDir("w1")
	os.MkdirAll(workerLib, 0o755)
	os.WriteFile(filepath.Join(workerLib, "marker"), []byte("x"), 0o644)

	EvictWorker(context.Background(), srv, "w1", telemetry.ReasonGraceful)

	// Worker library should be removed.
	if _, err := os.Stat(workerLib); !os.IsNotExist(err) {
		t.Error("worker lib dir should be removed after eviction")
	}
}

func TestStartupCleanupPassiveSkipsDestructiveOps(t *testing.T) {
	srv, be := testServer(t)
	bsp := srv.Config.Storage.BundleServerPath

	// Seed managed resources — should survive in passive mode.
	be.SetManagedResources([]backend.ManagedResource{
		{ID: "container-1", Kind: backend.ResourceContainer},
	})

	// Create worker token directory — should survive in passive mode.
	tokenDir := filepath.Join(bsp, ".worker-tokens", "w-live")
	os.MkdirAll(tokenDir, 0o755)
	os.WriteFile(filepath.Join(tokenDir, "token"), []byte("tok"), 0o644)

	// Create orphaned staging directory — should still be cleaned.
	storeRoot := filepath.Join(bsp, ".pkg-store")
	os.MkdirAll(storeRoot, 0o755)
	srv.PkgStore = pkgstore.NewStore(storeRoot)
	stagingDir := filepath.Join(storeRoot, ".staging", "orphan-1")
	os.MkdirAll(stagingDir, 0o755)
	os.WriteFile(filepath.Join(stagingDir, "data"), []byte("stale"), 0o644)

	// Create orphaned transfer directory — should still be cleaned.
	transferDir := filepath.Join(bsp, ".transfers", "old-worker")
	os.MkdirAll(transferDir, 0o755)
	os.WriteFile(filepath.Join(transferDir, "board.json"), []byte("{}"), 0o644)

	// Create a stale build — should still be failed.
	app, err := srv.DB.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := srv.DB.CreateBundle("bundle-1", app.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.DB.UpdateBundleStatus(bundle.ID, "building"); err != nil {
		t.Fatal(err)
	}

	if err := StartupCleanup(context.Background(), srv, true); err != nil {
		t.Fatal(err)
	}

	// Destructive ops must be skipped:
	resources, _ := be.ListManaged(context.Background())
	if len(resources) != 1 {
		t.Errorf("passive mode should preserve managed resources, got %d", len(resources))
	}
	if _, err := os.Stat(tokenDir); os.IsNotExist(err) {
		t.Error("passive mode should preserve worker token directories")
	}

	// Non-destructive cleanup must still run:
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Error("staging dir should be removed even in passive mode")
	}
	if _, err := os.Stat(transferDir); !os.IsNotExist(err) {
		t.Error("transfer dir should be removed even in passive mode")
	}
	b, err := srv.DB.GetBundle(bundle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "failed" {
		t.Errorf("stale builds should be failed even in passive mode, got %q", b.Status)
	}
}

func TestStartupCleanupRemovesOrphans(t *testing.T) {
	srv, be := testServer(t)

	be.SetManagedResources([]backend.ManagedResource{
		{ID: "container-1", Kind: backend.ResourceContainer},
		{ID: "network-1", Kind: backend.ResourceNetwork},
	})

	if err := StartupCleanup(context.Background(), srv, false); err != nil {
		t.Fatal(err)
	}

	resources, _ := be.ListManaged(context.Background())
	if len(resources) != 0 {
		t.Errorf("expected 0 managed resources after cleanup, got %d", len(resources))
	}
}

func TestStartupCleanupRemovesOrphanDirs(t *testing.T) {
	srv, _ := testServer(t)
	bsp := srv.Config.Storage.BundleServerPath

	// Set up PkgStore with orphaned staging directories.
	storeRoot := filepath.Join(bsp, ".pkg-store")
	os.MkdirAll(storeRoot, 0o755)
	srv.PkgStore = pkgstore.NewStore(storeRoot)

	stagingDir := filepath.Join(storeRoot, ".staging", "orphan-1")
	os.MkdirAll(stagingDir, 0o755)
	os.WriteFile(filepath.Join(stagingDir, "data"), []byte("stale"), 0o644)

	// Create orphaned transfer directories.
	transferDir := filepath.Join(bsp, ".transfers", "old-worker")
	os.MkdirAll(transferDir, 0o755)
	os.WriteFile(filepath.Join(transferDir, "board.json"), []byte("{}"), 0o644)

	// Create orphaned token directories.
	tokenDir := filepath.Join(bsp, ".worker-tokens", "old-worker")
	os.MkdirAll(tokenDir, 0o755)
	os.WriteFile(filepath.Join(tokenDir, "token"), []byte("tok"), 0o644)

	if err := StartupCleanup(context.Background(), srv, false); err != nil {
		t.Fatal(err)
	}

	// All orphaned directories should be removed.
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Error("staging dir should be removed")
	}
	if _, err := os.Stat(transferDir); !os.IsNotExist(err) {
		t.Error("transfer dir should be removed")
	}
	if _, err := os.Stat(tokenDir); !os.IsNotExist(err) {
		t.Error("token dir should be removed")
	}
}

func TestEvictWorkerCleansUpTransferAndToken(t *testing.T) {
	srv, be := testServer(t)
	spawnWorker(t, srv, be, "w1", "app1")
	bsp := srv.Config.Storage.BundleServerPath

	// Create transfer and token directories for the worker.
	transferDir := filepath.Join(bsp, ".transfers", "w1")
	os.MkdirAll(transferDir, 0o755)
	os.WriteFile(filepath.Join(transferDir, "board.json"), []byte("{}"), 0o644)

	tokenDir := filepath.Join(bsp, ".worker-tokens", "w1")
	os.MkdirAll(tokenDir, 0o755)
	os.WriteFile(filepath.Join(tokenDir, "token"), []byte("tok"), 0o644)

	EvictWorker(context.Background(), srv, "w1", telemetry.ReasonGraceful)

	if _, err := os.Stat(transferDir); !os.IsNotExist(err) {
		t.Error("transfer dir should be removed after eviction")
	}
	if _, err := os.Stat(tokenDir); !os.IsNotExist(err) {
		t.Error("token dir should be removed after eviction")
	}
}

func TestStartupCleanupFailsStaleBuilds(t *testing.T) {
	srv, _ := testServer(t)

	app, err := srv.DB.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := srv.DB.CreateBundle("bundle-1", app.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.DB.UpdateBundleStatus(bundle.ID, "building"); err != nil {
		t.Fatal(err)
	}

	if err := StartupCleanup(context.Background(), srv, false); err != nil {
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

func TestStartupCleanupPassiveKeepsWorkersWithLiveContainers(t *testing.T) {
	srv, be := testServer(t)

	// Seed two workers in Redis, as if a previous server spawned them.
	srv.Workers.Set("w-keep", server.ActiveWorker{AppID: "app1"})
	srv.Registry.Set("w-keep", "127.0.0.1:9001")
	srv.Workers.Set("w-stale", server.ActiveWorker{AppID: "app2"})
	srv.Registry.Set("w-stale", "127.0.0.1:9002")

	// Only w-keep still has a live container. The container ID is a
	// Docker-style hex hash, distinct from the worker UUID — this is the
	// mismatch that issue #156 exposed.
	be.SetManagedResources([]backend.ManagedResource{
		{
			ID:     "c0ffee1234",
			Kind:   backend.ResourceContainer,
			Labels: map[string]string{"dev.blockyard/worker-id": "w-keep"},
		},
	})

	if err := StartupCleanup(context.Background(), srv, true); err != nil {
		t.Fatal(err)
	}

	if _, ok := srv.Workers.Get("w-keep"); !ok {
		t.Error("worker with live container should be kept after passive reconciliation")
	}
	if _, ok := srv.Registry.Get("w-keep"); !ok {
		t.Error("registry entry for live worker should be kept")
	}
	if _, ok := srv.Workers.Get("w-stale"); ok {
		t.Error("worker without a live container should be removed")
	}
}

func TestEvictDrainedWorkersEvictsZeroSessions(t *testing.T) {
	srv, be := testServer(t)
	spawnWorker(t, srv, be, "w-drain", "app1")
	srv.Workers.SetDraining("w-drain")

	// No sessions for w-drain → should be evicted.
	evictDrainedWorkers(context.Background(), srv)

	if _, ok := srv.Workers.Get("w-drain"); ok {
		t.Error("draining worker with zero sessions should be evicted")
	}
}

func TestEvictDrainedWorkersKeepsWithSessions(t *testing.T) {
	srv, be := testServer(t)
	spawnWorker(t, srv, be, "w-drain", "app1")
	srv.Workers.SetDraining("w-drain")
	srv.Sessions.Set("sess1", session.Entry{WorkerID: "w-drain"})

	evictDrainedWorkers(context.Background(), srv)

	if _, ok := srv.Workers.Get("w-drain"); !ok {
		t.Error("draining worker with active sessions should NOT be evicted")
	}
}

func TestEvictDrainedWorkersSkipsNonDraining(t *testing.T) {
	srv, be := testServer(t)
	spawnWorker(t, srv, be, "w-healthy", "app1")

	evictDrainedWorkers(context.Background(), srv)

	if _, ok := srv.Workers.Get("w-healthy"); !ok {
		t.Error("non-draining worker should not be evicted")
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
	bundle, err := srv.DB.CreateBundle("bundle-1", app.ID, "", false)
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

func TestSweepDeletedAppsPurgesExpired(t *testing.T) {
	srv, be := testServer(t)
	srv.Config.Storage.SoftDeleteRetention = config.Duration{Duration: 1 * time.Second}

	// Create an app with a bundle, then soft-delete it.
	app, err := srv.DB.CreateApp("expired-app", "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Spawn a worker for the app so StopAppSync has something to stop.
	spawnWorker(t, srv, be, "w-exp", app.ID)

	if err := srv.DB.SoftDeleteApp(app.ID); err != nil {
		t.Fatal(err)
	}

	// Wait for retention to expire. RFC3339 has second granularity so
	// both deleted_at and cutoff truncate — need retention + 1s margin.
	time.Sleep(2200 * time.Millisecond)

	sweepDeletedApps(srv)

	// App should be hard-deleted (GetAppIncludeDeleted returns nil, nil).
	got, _ := srv.DB.GetAppIncludeDeleted(app.ID)
	if got != nil {
		t.Error("expected app to be hard-deleted after sweep")
	}

	// Worker should be evicted.
	if be.HasWorker("w-exp") {
		t.Error("expected worker to be stopped after sweep")
	}
}

func TestSweepDeletedAppsSkipsUnexpired(t *testing.T) {
	srv, _ := testServer(t)
	srv.Config.Storage.SoftDeleteRetention = config.Duration{Duration: 1 * time.Hour}

	app, err := srv.DB.CreateApp("recent-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.DB.SoftDeleteApp(app.ID); err != nil {
		t.Fatal(err)
	}

	sweepDeletedApps(srv)

	// App should still exist (retention not expired).
	got, err := srv.DB.GetAppIncludeDeleted(app.ID)
	if err != nil {
		t.Fatal("expected app to survive sweep, got error:", err)
	}
	if got.DeletedAt == nil {
		t.Error("expected app to still be soft-deleted")
	}
}

func TestSweepDeletedAppsNoop(t *testing.T) {
	srv, _ := testServer(t)
	srv.Config.Storage.SoftDeleteRetention = config.Duration{Duration: 1 * time.Hour}

	// No soft-deleted apps — should not panic.
	sweepDeletedApps(srv)
}

func TestSpawnSoftDeleteSweeperStopsOnCancel(t *testing.T) {
	srv, _ := testServer(t)
	srv.Config.Storage.SoftDeleteRetention = config.Duration{Duration: 50 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		SpawnSoftDeleteSweeper(ctx, srv)
		close(done)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Returned — success.
	case <-time.After(2 * time.Second):
		t.Fatal("SpawnSoftDeleteSweeper did not return after context cancel")
	}
}

func TestSpawnSoftDeleteSweeperZeroRetentionNoop(t *testing.T) {
	srv, _ := testServer(t)
	srv.Config.Storage.SoftDeleteRetention = config.Duration{Duration: 0}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		SpawnSoftDeleteSweeper(ctx, srv)
		close(done)
	}()

	// With zero retention, the sweeper just blocks on ctx.Done().
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Returned — success.
	case <-time.After(2 * time.Second):
		t.Fatal("SpawnSoftDeleteSweeper did not return after context cancel (zero retention)")
	}
}

func TestSpawnSoftDeleteSweeperSweepsOnTick(t *testing.T) {
	srv, _ := testServer(t)
	// Use a 1s retention so RFC3339 cutoff comparison works reliably.
	srv.Config.Storage.SoftDeleteRetention = config.Duration{Duration: 1 * time.Second}

	// Create and soft-delete an app.
	app, err := srv.DB.CreateApp("sweep-tick-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.DB.SoftDeleteApp(app.ID); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		SpawnSoftDeleteSweeper(ctx, srv)
		close(done)
	}()

	// Wait for retention to expire (1s) and for the ticker to fire (also 1s).
	time.Sleep(2500 * time.Millisecond)
	cancel()
	<-done

	got, _ := srv.DB.GetAppIncludeDeleted(app.ID)
	if got != nil {
		t.Error("expected app to be purged by sweeper")
	}
}

func TestStopAppSyncNoWorkers(t *testing.T) {
	srv, _ := testServer(t)

	// Should be a no-op — must not panic.
	StopAppSync(srv, "nonexistent-app")
}

func TestStopAppSyncDrainsAndEvicts(t *testing.T) {
	srv, be := testServer(t)
	srv.Config.Server.ShutdownTimeout = config.Duration{Duration: 100 * time.Millisecond}

	spawnWorker(t, srv, be, "w1", "app1")
	spawnWorker(t, srv, be, "w2", "app1")
	srv.Sessions.Set("sess1", session.Entry{WorkerID: "w1"})

	StopAppSync(srv, "app1")

	if len(srv.Workers.ForApp("app1")) != 0 {
		t.Errorf("expected all workers for app1 evicted, got %d", len(srv.Workers.ForApp("app1")))
	}
	if be.HasWorker("w1") || be.HasWorker("w2") {
		t.Error("expected backend workers to be stopped")
	}
}

func TestPurgeAppWithBundles(t *testing.T) {
	srv, _ := testServer(t)

	app, err := srv.DB.CreateApp("purge-app", "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Create bundles.
	srv.DB.CreateBundle("b-1", app.ID, "", false)
	srv.DB.UpdateBundleStatus("b-1", "ready")
	srv.DB.SetActiveBundle(app.ID, "b-1")
	srv.DB.CreateBundle("b-2", app.ID, "", false)
	srv.DB.UpdateBundleStatus("b-2", "ready")

	PurgeApp(srv, app)

	// App should be hard-deleted.
	got, _ := srv.DB.GetAppIncludeDeleted(app.ID)
	if got != nil {
		t.Error("expected app to be hard-deleted")
	}

	// Bundles should be gone.
	bundles, _ := srv.DB.ListBundlesByApp(app.ID)
	if len(bundles) != 0 {
		t.Errorf("expected 0 bundles, got %d", len(bundles))
	}
}
