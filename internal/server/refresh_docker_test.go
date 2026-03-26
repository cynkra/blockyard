//go:build docker_test

package server_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	dockerbe "github.com/cynkra/blockyard/internal/backend/docker"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/logstore"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/pkgstore"
	"github.com/cynkra/blockyard/internal/registry"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/testutil"
)

const (
	testImage      = "ghcr.io/rocker-org/r-ver:4.4.3"
	testPakVersion = "stable"
)

func dockerTestConfig() *config.DockerConfig {
	return &config.DockerConfig{
		Socket:     "/var/run/docker.sock",
		Image:      testImage,
		ShinyPort:  8080,
		PakVersion: testPakVersion,
	}
}

// setupDockerServer creates a Server backed by a real Docker backend
// for integration testing of refresh and package install flows.
func setupDockerServer(t *testing.T) (*server.Server, *dockerbe.DockerBackend) {
	t.Helper()
	ctx := context.Background()
	basePath := t.TempDir()

	be, err := dockerbe.New(ctx, dockerTestConfig(), basePath)
	if err != nil {
		t.Fatalf("New docker backend: %v", err)
	}

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	storeRoot := filepath.Join(basePath, ".pkg-store")
	os.MkdirAll(storeRoot, 0o755)

	cfg := &config.Config{
		Docker: *dockerTestConfig(),
		Storage: config.StorageConfig{
			BundleServerPath: basePath,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
		},
		Proxy: config.ProxyConfig{
			MaxWorkers:         10,
			WorkerStartTimeout: config.Duration{Duration: 60 * time.Second},
			TransferTimeout:    config.Duration{Duration: 30 * time.Second},
		},
		Server: config.ServerConfig{
			Bind: ":8080",
		},
	}

	srv := &server.Server{
		Config:   cfg,
		Backend:  be,
		DB:       database,
		Workers:  server.NewWorkerMap(),
		Sessions: session.NewStore(),
		Registry: registry.New(),
		Tasks:    task.NewStore(),
		LogStore: logstore.NewStore(),
		PkgStore: pkgstore.NewStore(storeRoot),
		EvictWorkerFn: func(ctx context.Context, srv *server.Server, workerID string) {
			ops.EvictWorker(ctx, srv, workerID)
		},
	}

	return srv, be
}

// deployUnpinnedBundle creates an app, writes a bundle with an unpinned
// manifest (DESCRIPTION-based), runs SpawnRestore, waits for build to
// complete, and starts a worker. Returns the app row and worker ID.
func deployUnpinnedBundle(t *testing.T, srv *server.Server) (*db.AppRow, string) {
	t.Helper()
	ctx := context.Background()

	app, err := srv.DB.CreateApp("refresh-test-app", "")
	if err != nil {
		t.Fatal(err)
	}
	bundleID := uuid.New().String()[:8]
	if _, err := srv.DB.CreateBundle(bundleID, app.ID); err != nil {
		t.Fatal(err)
	}

	// Write and unpack the bundle.
	archiveData := testutil.MakeBundle(t)
	paths := bundle.NewBundlePaths(srv.Config.Storage.BundleServerPath, app.ID, bundleID)
	if err := bundle.WriteArchive(paths, bytes.NewReader(archiveData)); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	if err := bundle.UnpackArchive(paths); err != nil {
		t.Fatalf("UnpackArchive: %v", err)
	}
	if err := bundle.CreateLibraryDir(paths); err != nil {
		t.Fatalf("CreateLibraryDir: %v", err)
	}

	// Write an UNPINNED manifest: uses DESCRIPTION (not pinned packages).
	// This means refresh can re-resolve dependencies.
	manifest := `{"version":1,"platform":"4.4.3","metadata":{"appmode":"shiny","entrypoint":"app.R"},"repositories":[{"Name":"CRAN","URL":"https://p3m.dev/cran/latest"}],"description":{"Imports":"mime"},"files":{"app.R":{"checksum":"abc"}}}`
	os.WriteFile(filepath.Join(paths.Unpacked, "manifest.json"), []byte(manifest), 0o644)
	os.WriteFile(filepath.Join(paths.Unpacked, "DESCRIPTION"),
		[]byte("Package: testapp\nVersion: 0.1.0\nImports:\n    mime\n"), 0o644)

	// Run the full restore pipeline (pak build + store ingest).
	pakCachePath := filepath.Join(srv.Config.Storage.BundleServerPath, ".pak-cache")
	taskID := uuid.New().String()
	sender := srv.Tasks.Create(taskID, app.ID)

	bundle.SpawnRestore(bundle.RestoreParams{
		Backend:      srv.Backend,
		DB:           srv.DB,
		Tasks:        srv.Tasks,
		Sender:       sender,
		AppID:        app.ID,
		BundleID:     bundleID,
		Paths:        paths,
		Image:        testImage,
		PakVersion:   testPakVersion,
		PakCachePath: pakCachePath,
		Retention:    5,
		BasePath:     srv.Config.Storage.BundleServerPath,
	})

	// Wait for build to complete.
	_, _, done, ok := srv.Tasks.Subscribe(taskID)
	if !ok {
		t.Fatal("task not found")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Minute):
		t.Fatal("restore timed out after 5 minutes")
	}

	status, _ := srv.Tasks.Status(taskID)
	if status != task.Completed {
		snap, _, _, _ := srv.Tasks.Subscribe(taskID)
		t.Fatalf("restore failed (status=%d); task logs:\n%s", status, strings.Join(snap, "\n"))
	}

	// Re-read app to get active bundle.
	appRow, err := srv.DB.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if appRow.ActiveBundle == nil {
		t.Fatal("active bundle not set after restore")
	}

	// Spawn a worker.
	workerID := "refresh-worker-" + uuid.New().String()[:8]
	hostPaths := bundle.NewBundlePaths(srv.Config.Storage.BundleServerPath, app.ID, bundleID)

	// Assemble library from the store manifest if it exists.
	storeManifestPath := filepath.Join(hostPaths.Base, "store-manifest.json")
	libDir := srv.PkgStore.WorkerLibDir(workerID)
	if _, err := os.Stat(storeManifestPath); err == nil {
		storeManifest, err := pkgstore.ReadStoreManifest(storeManifestPath)
		if err == nil && len(storeManifest) > 0 {
			srv.PkgStore.AssembleLibrary(libDir, storeManifest)
		}
	}

	spec := backend.WorkerSpec{
		AppID:       app.ID,
		WorkerID:    workerID,
		Image:       testImage,
		Cmd:         []string{"R", "--no-save", "-e", "cat('worker ok'); Sys.sleep(300)"},
		BundlePath:  hostPaths.Unpacked,
		LibraryPath: hostPaths.Library,
		LibDir:      libDir,
		WorkerMount: "/app",
		ShinyPort:   8080,
		Labels:      map[string]string{},
	}

	if err := srv.Backend.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn worker: %v", err)
	}
	t.Cleanup(func() { srv.Backend.Stop(context.Background(), workerID) })

	addr, err := srv.Backend.Addr(ctx, workerID)
	if err != nil {
		t.Fatalf("Addr: %v", err)
	}

	srv.Workers.Set(workerID, server.ActiveWorker{
		AppID:    app.ID,
		BundleID: bundleID,
	})
	srv.Registry.Set(workerID, addr)

	return appRow, workerID
}

// TestRefreshFlow_Docker exercises the full refresh pipeline with real
// Docker containers: deploy unpinned bundle → RunRefresh → verify new
// worker spawned and old worker draining.
func TestRefreshFlow_Docker(t *testing.T) {
	srv, _ := setupDockerServer(t)

	// Deploy an unpinned app and start a worker.
	appRow, oldWorkerID := deployUnpinnedBundle(t, srv)
	t.Logf("deployed app %s with worker %s", appRow.ID, oldWorkerID)

	// Read the manifest for RunRefresh.
	bundlePaths := srv.BundlePaths(appRow.ID, *appRow.ActiveBundle)
	manifestPath := filepath.Join(bundlePaths.Base, "manifest.json")
	m, err := manifest.Read(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if m.IsPinned() {
		t.Fatal("expected unpinned manifest for refresh test")
	}

	// Run refresh.
	taskID := uuid.New().String()
	sender := srv.Tasks.Create(taskID, appRow.ID)

	changed := srv.RunRefresh(context.Background(), appRow, m, sender)
	t.Logf("RunRefresh returned changed=%v", changed)

	status, _ := srv.Tasks.Status(taskID)
	if status == task.Failed {
		snap, _, _, _ := srv.Tasks.Subscribe(taskID)
		t.Fatalf("refresh failed; task logs:\n%s", strings.Join(snap, "\n"))
	}

	// Whether or not dependencies changed, the task should complete.
	if status != task.Completed {
		t.Errorf("expected task Completed, got %d", status)
	}

	// If dependencies changed, old worker should be draining and a new
	// worker should exist.
	if changed {
		if !srv.Workers.IsDraining(appRow.ID) {
			t.Error("expected old workers to be draining after refresh")
		}
		count := srv.Workers.CountForApp(appRow.ID)
		if count < 2 {
			t.Errorf("expected >=2 workers (old draining + new), got %d", count)
		}
		t.Log("refresh spawned new worker — drain-and-replace working")
	} else {
		t.Log("dependencies unchanged — no worker replacement (expected for idempotent refresh)")
	}

	// Clean up spawned workers.
	for _, wid := range srv.Workers.ForApp(appRow.ID) {
		if wid != oldWorkerID {
			srv.Backend.Stop(context.Background(), wid)
		}
	}
}

// TestRollbackFlow_Docker exercises rollback after a refresh.
func TestRollbackFlow_Docker(t *testing.T) {
	srv, _ := setupDockerServer(t)

	appRow, oldWorkerID := deployUnpinnedBundle(t, srv)
	t.Logf("deployed app %s with worker %s", appRow.ID, oldWorkerID)

	bundlePaths := srv.BundlePaths(appRow.ID, *appRow.ActiveBundle)

	// Simulate a previous refresh by copying current store-manifest as .prev.
	currentManifest := filepath.Join(bundlePaths.Base, "store-manifest.json")
	if _, err := os.Stat(currentManifest); err == nil {
		data, _ := os.ReadFile(currentManifest)
		os.WriteFile(filepath.Join(bundlePaths.Base, "store-manifest.json.prev"), data, 0o644)
		os.WriteFile(filepath.Join(bundlePaths.Base, "store-manifest.json.build"), data, 0o644)
	}

	// Test rollback to previous refresh (default target).
	taskID := uuid.New().String()
	sender := srv.Tasks.Create(taskID, appRow.ID)
	srv.RunRollback(context.Background(), appRow, "", sender)

	status, _ := srv.Tasks.Status(taskID)
	if status != task.Completed {
		snap, _, _, _ := srv.Tasks.Subscribe(taskID)
		t.Fatalf("rollback failed; task logs:\n%s", strings.Join(snap, "\n"))
	}
	t.Log("rollback to previous refresh succeeded")

	// Test rollback to original build.
	taskID2 := uuid.New().String()
	sender2 := srv.Tasks.Create(taskID2, appRow.ID)
	srv.RunRollback(context.Background(), appRow, "build", sender2)

	status2, _ := srv.Tasks.Status(taskID2)
	if status2 != task.Completed {
		snap, _, _, _ := srv.Tasks.Subscribe(taskID2)
		t.Fatalf("rollback to build failed; task logs:\n%s", strings.Join(snap, "\n"))
	}
	t.Log("rollback to original build succeeded")

	// Clean up.
	for _, wid := range srv.Workers.ForApp(appRow.ID) {
		srv.Backend.Stop(context.Background(), wid)
	}
}

