package server

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
	"github.com/cynkra/blockyard/internal/logstore"
	"github.com/cynkra/blockyard/internal/pkgstore"
	"github.com/cynkra/blockyard/internal/registry"
	"github.com/cynkra/blockyard/internal/session"
	"github.com/cynkra/blockyard/internal/task"
)

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	os.WriteFile(src, []byte("hello world"), 0o644)

	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Errorf("got %q, want %q", data, "hello world")
	}
}

func TestCopyFile_SrcMissing(t *testing.T) {
	dir := t.TempDir()
	err := copyFile(filepath.Join(dir, "missing"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Error("expected error for missing source")
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	os.WriteFile(f, []byte("x"), 0o644)

	if !fileExists(f) {
		t.Error("expected true for existing file")
	}
	if fileExists(filepath.Join(dir, "nope")) {
		t.Error("expected false for missing file")
	}
	if fileExists(dir) {
		t.Error("expected false for directory")
	}
}

func TestDirExists(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	os.WriteFile(f, []byte("x"), 0o644)

	if !dirExists(dir) {
		t.Error("expected true for existing directory")
	}
	if dirExists(filepath.Join(dir, "nope")) {
		t.Error("expected false for missing path")
	}
	if dirExists(f) {
		t.Error("expected false for file")
	}
}

func TestTransferDir(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Storage: config.StorageConfig{
				BundleServerPath: "/data/bundles",
			},
		},
	}
	got := srv.TransferDir("w-123")
	want := "/data/bundles/.transfers/w-123"
	if got != want {
		t.Errorf("TransferDir = %q, want %q", got, want)
	}
}

func testServerWithMock(t *testing.T) (*Server, *mock.MockBackend) {
	t.Helper()
	dir := t.TempDir()
	be := mock.New()

	storeRoot := filepath.Join(dir, "store")
	os.MkdirAll(storeRoot, 0o755)

	cfg := &config.Config{
		Storage: config.StorageConfig{
			BundleServerPath:  dir,
			BundleWorkerPath:  "/app",
		},
		Server: config.ServerConfig{
			Bind: ":8080",
		},
		Docker: config.DockerConfig{
			Image:     "test-image:latest",
			ShinyPort: 3838,
		},
		Proxy: config.ProxyConfig{
			WorkerStartTimeout: config.Duration{Duration: 5 * time.Second},
			TransferTimeout:    config.Duration{Duration: 2 * time.Second},
		},
	}

	srv := &Server{
		Config:   cfg,
		Backend:  be,
		Workers:  NewMemoryWorkerMap(),
		Sessions: session.NewMemoryStore(),
		Registry: registry.NewMemoryRegistry(),
		Tasks:    task.NewStore(),
		LogStore: logstore.NewStore(),
		PkgStore: pkgstore.NewStore(storeRoot),
		EvictWorkerFn: func(_ context.Context, _ *Server, _ string) {},
	}

	return srv, be
}

func TestDefaultWorkerSpec(t *testing.T) {
	srv, _ := testServerWithMock(t)

	app := &db.AppRow{ID: "app-1", Name: "test-app"}
	spec := srv.defaultWorkerSpec(app, "w-1", "/lib/w-1", "bundle-abc")
	if spec.AppID != "app-1" {
		t.Errorf("AppID = %q, want %q", spec.AppID, "app-1")
	}
	if spec.WorkerID != "w-1" {
		t.Errorf("WorkerID = %q, want %q", spec.WorkerID, "w-1")
	}
	if spec.Image != "test-image:latest" {
		t.Errorf("Image = %q, want %q", spec.Image, "test-image:latest")
	}
	if spec.ShinyPort != 3838 {
		t.Errorf("ShinyPort = %d, want %d", spec.ShinyPort, 3838)
	}
	if spec.LibDir != "/lib/w-1" {
		t.Errorf("LibDir = %q, want %q", spec.LibDir, "/lib/w-1")
	}
	if spec.Labels["dev.blockyard/role"] != "worker" {
		t.Errorf("role label = %q, want %q", spec.Labels["dev.blockyard/role"], "worker")
	}
	// Env should include BLOCKYARD_API_URL.
	if spec.Env["BLOCKYARD_API_URL"] == "" {
		t.Error("expected BLOCKYARD_API_URL in env")
	}
}

func TestBuildTransferWorkerSpec(t *testing.T) {
	srv, _ := testServerWithMock(t)

	app := &db.AppRow{ID: "app-1", Name: "test-app"}
	spec := srv.buildTransferWorkerSpec(app, "w-2", "/lib/w-2", "/transfer/w-1", "bundle-abc")
	if spec.TransferDir != "/transfer/w-1" {
		t.Errorf("TransferDir = %q, want %q", spec.TransferDir, "/transfer/w-1")
	}
	if spec.Env["BLOCKYARD_TRANSFER_PATH"] != "/transfer/board.json" {
		t.Errorf("BLOCKYARD_TRANSFER_PATH = %q, want %q",
			spec.Env["BLOCKYARD_TRANSFER_PATH"], "/transfer/board.json")
	}
}

func TestBuildTransferWorkerSpec_EmptyTransferDir(t *testing.T) {
	srv, _ := testServerWithMock(t)

	app := &db.AppRow{ID: "app-1", Name: "test-app"}
	spec := srv.buildTransferWorkerSpec(app, "w-2", "/lib/w-2", "", "bundle-abc")
	if _, ok := spec.Env["BLOCKYARD_TRANSFER_PATH"]; ok {
		t.Error("expected no BLOCKYARD_TRANSFER_PATH when transfer dir is empty")
	}
}

func TestWaitHealthy_ImmediateSuccess(t *testing.T) {
	srv, be := testServerWithMock(t)
	be.HealthOK.Store(true)

	// Spawn a mock worker so HealthCheck finds it.
	be.Spawn(context.Background(), mockWorkerSpec("w-1"))

	err := srv.waitHealthy(context.Background(), "w-1")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestWaitHealthy_Timeout(t *testing.T) {
	srv, be := testServerWithMock(t)
	srv.Config.Proxy.WorkerStartTimeout = config.Duration{Duration: 200 * time.Millisecond}
	be.HealthOK.Store(false)

	// Spawn a mock worker that is never healthy.
	be.Spawn(context.Background(), mockWorkerSpec("w-1"))

	err := srv.waitHealthy(context.Background(), "w-1")
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestWatchTransfer_BoardFileAppears(t *testing.T) {
	srv, be := testServerWithMock(t)
	srv.Config.Proxy.TransferTimeout = config.Duration{Duration: 5 * time.Second}
	srv.Config.Proxy.WorkerStartTimeout = config.Duration{Duration: 2 * time.Second}

	// Set up old worker in the map.
	srv.Workers.Set("old-w", ActiveWorker{AppID: "app-1", BundleID: "b-1"})

	// Create a store with a package so AssembleLibrary works.
	store := srv.PkgStore
	store.SetPlatform("test-platform")
	pkgDir := store.Path("shiny", "src1", "cfg1")
	os.MkdirAll(pkgDir, 0o755)
	os.WriteFile(filepath.Join(pkgDir, "DESCRIPTION"), []byte("Package: shiny"), 0o644)

	// Write store-manifest in a transfer dir.
	transferDir := t.TempDir()
	storeManifestPath := filepath.Join(transferDir, "store-manifest.json")
	pkgstore.WriteStoreManifest(transferDir, map[string]string{"shiny": "src1/cfg1"})

	// Write the board file to trigger completeTransfer.
	boardPath := filepath.Join(transferDir, "board.json")
	os.WriteFile(boardPath, []byte(`{}`), 0o644)

	// Watch should detect the board file and complete the transfer.
	done := make(chan struct{})
	go func() {
		srv.watchTransfer(context.Background(), "app-1", "old-w",
			storeManifestPath, transferDir)
		close(done)
	}()

	select {
	case <-done:
		// Verify a new worker was spawned.
		if be.WorkerCount() == 0 {
			t.Error("expected new worker to be spawned")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watchTransfer did not complete in time")
	}
}

func TestWatchTransfer_Timeout(t *testing.T) {
	srv, _ := testServerWithMock(t)
	srv.Config.Proxy.TransferTimeout = config.Duration{Duration: 200 * time.Millisecond}

	transferDir := t.TempDir()
	storeManifestPath := filepath.Join(transferDir, "store-manifest.json")
	os.WriteFile(storeManifestPath, []byte(`{}`), 0o644)

	// No board file → should time out.
	done := make(chan struct{})
	go func() {
		srv.watchTransfer(context.Background(), "app-1", "w-1",
			storeManifestPath, transferDir)
		close(done)
	}()

	select {
	case <-done:
		// Good — timed out as expected.
	case <-time.After(5 * time.Second):
		t.Fatal("watchTransfer did not time out")
	}
}

func TestWatchTransfer_ContextCancelled(t *testing.T) {
	srv, _ := testServerWithMock(t)
	srv.Config.Proxy.TransferTimeout = config.Duration{Duration: 30 * time.Second}

	transferDir := t.TempDir()
	storeManifestPath := filepath.Join(transferDir, "store-manifest.json")
	os.WriteFile(storeManifestPath, []byte(`{}`), 0o644)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		srv.watchTransfer(ctx, "app-1", "w-1", storeManifestPath, transferDir)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// Good — returned on context cancel.
	case <-time.After(5 * time.Second):
		t.Fatal("watchTransfer did not return on cancel")
	}
}

func TestHandleTransfer(t *testing.T) {
	srv, _ := testServerWithMock(t)

	// Write a store-manifest file.
	stageDir := t.TempDir()
	manifestPath := filepath.Join(stageDir, "store-manifest.json")
	pkgstore.WriteStoreManifest(stageDir, map[string]string{"shiny": "src1/cfg1"})

	// In production, defaultWorkerSpec creates the transfer dir at spawn time.
	os.MkdirAll(srv.TransferDir("w-1"), 0o755)

	resp, err := srv.handleTransfer(context.Background(), "app-1", "w-1", manifestPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "transfer" {
		t.Errorf("Status = %q, want %q", resp.Status, "transfer")
	}
	if resp.TransferPath != "/transfer" {
		t.Errorf("TransferPath = %q, want %q", resp.TransferPath, "/transfer")
	}

	// Verify the store-manifest was copied to the transfer directory.
	transferManifest := filepath.Join(srv.TransferDir("w-1"), "store-manifest.json")
	if !fileExists(transferManifest) {
		t.Error("expected store-manifest.json in transfer directory")
	}
}

func mockWorkerSpec(id string) backend.WorkerSpec {
	return backend.WorkerSpec{WorkerID: id, AppID: "app-1"}
}

// ---------------------------------------------------------------------------
// T2: completeTransfer partial failure — new worker unhealthy
// ---------------------------------------------------------------------------

func TestCompleteTransfer_UnhealthyWorkerCleanup(t *testing.T) {
	srv, be := testServerWithMock(t)
	srv.Config.Proxy.TransferTimeout.Duration = 5 * time.Second
	srv.Config.Proxy.WorkerStartTimeout.Duration = 300 * time.Millisecond

	// Old worker must exist so completeTransfer doesn't bail at the guard.
	srv.Workers.Set("old-w", ActiveWorker{AppID: "app-1", BundleID: "b-1"})

	// Create a store package and write a store-manifest referencing it.
	store := srv.PkgStore
	store.SetPlatform("test-platform")
	pkgDir := store.Path("shiny", "src1", "cfg1")
	os.MkdirAll(pkgDir, 0o755)
	os.WriteFile(filepath.Join(pkgDir, "DESCRIPTION"), []byte("Package: shiny"), 0o644)
	metaPath := store.ConfigMetaPath("shiny", "src1", "cfg1")
	os.WriteFile(metaPath, []byte(`{}`), 0o644)

	transferDir := t.TempDir()
	pkgstore.WriteStoreManifest(transferDir, map[string]string{"shiny": "src1/cfg1"})
	storeManifestPath := filepath.Join(transferDir, "store-manifest.json")

	// New worker will spawn but never become healthy.
	be.HealthOK.Store(false)

	workerCountBefore := srv.Workers.Count()

	srv.completeTransfer(context.Background(), "app-1", "old-w",
		storeManifestPath, transferDir)

	// The new worker should have been cleaned up.
	// Workers should only contain the original old-w (not cleaned up here
	// because completeTransfer bails before reroute/evict on health failure).
	if srv.Workers.Count() != workerCountBefore {
		t.Errorf("Workers.Count = %d, want %d (ghost worker not cleaned up)",
			srv.Workers.Count(), workerCountBefore)
	}

	// The mock backend should have had the unhealthy worker stopped.
	// Since the new worker ID is random, check that be has no workers
	// (old-w was never spawned on the backend, and the new one was stopped).
	if be.WorkerCount() != 0 {
		t.Errorf("backend WorkerCount = %d, want 0 (unhealthy worker not stopped)",
			be.WorkerCount())
	}
}

// ---------------------------------------------------------------------------
// T3: completeTransfer when old worker already evicted
// ---------------------------------------------------------------------------

func TestCompleteTransfer_OldWorkerEvicted(t *testing.T) {
	srv, be := testServerWithMock(t)
	srv.Config.Proxy.WorkerStartTimeout.Duration = 2 * time.Second

	store := srv.PkgStore
	store.SetPlatform("test-platform")

	// Do NOT register "old-w" in Workers — simulates eviction.
	transferDir := t.TempDir()
	pkgstore.WriteStoreManifest(transferDir, map[string]string{"shiny": "src1/cfg1"})
	storeManifestPath := filepath.Join(transferDir, "store-manifest.json")

	srv.completeTransfer(context.Background(), "app-1", "old-w",
		storeManifestPath, transferDir)

	// No new worker should have been spawned.
	if be.WorkerCount() != 0 {
		t.Errorf("expected no workers spawned, got %d", be.WorkerCount())
	}
}

// ---------------------------------------------------------------------------
// T7: completeTransfer with missing store entries → abort
// ---------------------------------------------------------------------------

func TestCompleteTransfer_MissingStoreEntries(t *testing.T) {
	srv, be := testServerWithMock(t)
	srv.Config.Proxy.WorkerStartTimeout.Duration = 2 * time.Second

	// Old worker exists.
	srv.Workers.Set("old-w", ActiveWorker{AppID: "app-1", BundleID: "b-1"})

	store := srv.PkgStore
	store.SetPlatform("test-platform")

	// Write a store-manifest referencing a package NOT in the store.
	transferDir := t.TempDir()
	pkgstore.WriteStoreManifest(transferDir, map[string]string{"missing-pkg": "src1/cfg1"})
	storeManifestPath := filepath.Join(transferDir, "store-manifest.json")

	srv.completeTransfer(context.Background(), "app-1", "old-w",
		storeManifestPath, transferDir)

	// Should have aborted — no new worker spawned.
	if be.WorkerCount() != 0 {
		t.Errorf("expected no workers spawned (missing store entries), got %d",
			be.WorkerCount())
	}
}
