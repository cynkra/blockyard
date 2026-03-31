//go:build pak_test

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

func setupDockerServer(t *testing.T) *server.Server {
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
			WorkerStartTimeout: config.Duration{Duration: 2 * time.Second},
			TransferTimeout:    config.Duration{Duration: 30 * time.Second},
		},
		Server: config.ServerConfig{Bind: ":8080"},
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

	return srv
}

// deployUnpinnedBundle creates an app with an unpinned manifest, runs
// SpawnRestore, and spawns a worker. Returns app row and worker ID.
func deployUnpinnedBundle(t *testing.T, srv *server.Server) (*db.AppRow, string) {
	t.Helper()
	ctx := context.Background()

	app, err := srv.DB.CreateApp("refresh-test-app", "")
	if err != nil {
		t.Fatal(err)
	}
	bundleID := uuid.New().String()[:8]
	if _, err := srv.DB.CreateBundle(bundleID, app.ID, "", false); err != nil {
		t.Fatal(err)
	}

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

	// Write an unpinned manifest (DESCRIPTION-based, no pinned packages).
	manifestJSON := `{"version":1,"platform":"4.4.3","metadata":{"appmode":"shiny","entrypoint":"app.R"},"repositories":[{"Name":"CRAN","URL":"https://p3m.dev/cran/latest"}],"description":{"Imports":"mime"},"files":{"app.R":{"checksum":"abc"}}}`
	os.WriteFile(filepath.Join(paths.Unpacked, "manifest.json"), []byte(manifestJSON), 0o644)
	os.WriteFile(filepath.Join(paths.Unpacked, "DESCRIPTION"),
		[]byte("Package: testapp\nVersion: 0.1.0\nImports:\n    mime\n"), 0o644)

	pakCachePath := filepath.Join(srv.Config.Storage.BundleServerPath, ".pak-cache")
	taskID := uuid.New().String()
	sender := srv.Tasks.Create(taskID, app.ID)

	restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer restoreCancel()

	bundle.SpawnRestore(bundle.RestoreParams{
		Ctx:          restoreCtx,
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

	_, _, done, ok := srv.Tasks.Subscribe(taskID)
	if !ok {
		t.Fatal("task not found")
	}
	select {
	case <-done:
	case <-restoreCtx.Done():
		t.Fatal("restore timed out after 5 minutes")
	}

	status, _ := srv.Tasks.Status(taskID)
	if status != task.Completed {
		snap, _, _, _ := srv.Tasks.Subscribe(taskID)
		t.Fatalf("restore failed (status=%d); task logs:\n%s", status, strings.Join(snap, "\n"))
	}

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

	storeManifestPath := filepath.Join(hostPaths.Base, "store-manifest.json")
	libDir := srv.PkgStore.WorkerLibDir(workerID)
	if _, err := os.Stat(storeManifestPath); err == nil {
		sm, err := pkgstore.ReadStoreManifest(storeManifestPath)
		if err == nil && len(sm) > 0 {
			srv.PkgStore.AssembleLibrary(libDir, sm)
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

// TestRefreshAndRollback_Docker exercises the full refresh + rollback
// pipeline with real Docker containers. Structured as subtests sharing
// a single deployed app to avoid repeating the expensive pak build.
func TestRefreshAndRollback_Docker(t *testing.T) {
	srv := setupDockerServer(t)

	appRow, oldWorkerID := deployUnpinnedBundle(t, srv)
	t.Logf("deployed app %s with worker %s", appRow.ID, oldWorkerID)

	bundlePaths := srv.BundlePaths(appRow.ID, *appRow.ActiveBundle)

	t.Run("refresh", func(t *testing.T) {
		// The manifest.json lives in the Unpacked directory.
		manifestPath := filepath.Join(bundlePaths.Unpacked, "manifest.json")
		m, err := manifest.Read(manifestPath)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		if m.IsPinned() {
			t.Fatal("expected unpinned manifest for refresh test")
		}

		taskID := uuid.New().String()
		sender := srv.Tasks.Create(taskID, appRow.ID)
		changed := srv.RunRefresh(context.Background(), appRow, m, sender)
		t.Logf("RunRefresh returned changed=%v", changed)

		status, _ := srv.Tasks.Status(taskID)
		if status == task.Failed {
			snap, _, _, _ := srv.Tasks.Subscribe(taskID)
			t.Fatalf("refresh failed; task logs:\n%s", strings.Join(snap, "\n"))
		}
		if status != task.Completed {
			t.Errorf("expected task Completed, got %d", status)
		}

		if changed {
			if !srv.Workers.IsDraining(appRow.ID) {
				t.Error("expected old workers to be draining after refresh")
			}
			count := srv.Workers.CountForApp(appRow.ID)
			if count < 2 {
				t.Errorf("expected >=2 workers (old draining + new), got %d", count)
			}
			t.Log("refresh spawned new worker — drain-and-replace working")

			// Clean up extra workers for the next subtest.
			for _, wid := range srv.Workers.ForApp(appRow.ID) {
				if wid != oldWorkerID {
					srv.Backend.Stop(context.Background(), wid)
				}
			}
		} else {
			t.Log("dependencies unchanged — no worker replacement")
		}
	})

	t.Run("refresh_unchanged", func(t *testing.T) {
		// Run refresh again with the same DESCRIPTION. Since deps were
		// already resolved in the previous subtest, the store-manifest
		// should be identical and RunRefresh should return changed=false.
		manifestPath := filepath.Join(bundlePaths.Unpacked, "manifest.json")
		m, err := manifest.Read(manifestPath)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}

		taskID := uuid.New().String()
		sender := srv.Tasks.Create(taskID, appRow.ID)
		changed := srv.RunRefresh(context.Background(), appRow, m, sender)

		status, _ := srv.Tasks.Status(taskID)
		if status == task.Failed {
			snap, _, _, _ := srv.Tasks.Subscribe(taskID)
			t.Fatalf("refresh failed; task logs:\n%s", strings.Join(snap, "\n"))
		}
		if changed {
			t.Error("expected no dependency change on second refresh with same DESCRIPTION")
		}
		t.Log("second refresh correctly detected unchanged dependencies")
	})

	t.Run("rollback_prev", func(t *testing.T) {
		// After a refresh, store-manifest.json.prev should exist.
		prevPath := filepath.Join(bundlePaths.Base, "store-manifest.json.prev")
		if _, err := os.Stat(prevPath); os.IsNotExist(err) {
			// If refresh didn't change deps, create a .prev manually.
			currentPath := filepath.Join(bundlePaths.Base, "store-manifest.json")
			if _, err := os.Stat(currentPath); err == nil {
				data, _ := os.ReadFile(currentPath)
				os.WriteFile(prevPath, data, 0o644)
			} else {
				t.Skip("no store-manifest available for rollback test")
			}
		}

		taskID := uuid.New().String()
		sender := srv.Tasks.Create(taskID, appRow.ID)
		srv.RunRollback(context.Background(), appRow, "", sender)

		status, _ := srv.Tasks.Status(taskID)
		if status != task.Completed {
			snap, _, _, _ := srv.Tasks.Subscribe(taskID)
			t.Fatalf("rollback failed; task logs:\n%s", strings.Join(snap, "\n"))
		}
		t.Log("rollback to previous refresh succeeded")

		// Clean up spawned workers.
		for _, wid := range srv.Workers.ForApp(appRow.ID) {
			if wid != oldWorkerID {
				srv.Backend.Stop(context.Background(), wid)
			}
		}
	})

	t.Run("rollback_build", func(t *testing.T) {
		// Create a .build manifest if one doesn't exist.
		buildPath := filepath.Join(bundlePaths.Base, "store-manifest.json.build")
		if _, err := os.Stat(buildPath); os.IsNotExist(err) {
			currentPath := filepath.Join(bundlePaths.Base, "store-manifest.json")
			if _, err := os.Stat(currentPath); err == nil {
				data, _ := os.ReadFile(currentPath)
				os.WriteFile(buildPath, data, 0o644)
			} else {
				t.Skip("no store-manifest available for rollback_build test")
			}
		}

		taskID := uuid.New().String()
		sender := srv.Tasks.Create(taskID, appRow.ID)
		srv.RunRollback(context.Background(), appRow, "build", sender)

		status, _ := srv.Tasks.Status(taskID)
		if status != task.Completed {
			snap, _, _, _ := srv.Tasks.Subscribe(taskID)
			t.Fatalf("rollback to build failed; task logs:\n%s", strings.Join(snap, "\n"))
		}
		t.Log("rollback to original build succeeded")

		for _, wid := range srv.Workers.ForApp(appRow.ID) {
			if wid != oldWorkerID {
				srv.Backend.Stop(context.Background(), wid)
			}
		}
	})

	t.Run("refresh_bad_description", func(t *testing.T) {
		// Create a second app whose DESCRIPTION imports a nonexistent
		// package. pak should fail to resolve it, causing the build
		// container to exit non-zero.
		badBundleID := "bad-bundle"
		badAppID := "bad-deps-app"

		badPaths := srv.BundlePaths(badAppID, badBundleID)
		os.MkdirAll(badPaths.Unpacked, 0o755)
		os.WriteFile(filepath.Join(badPaths.Unpacked, "app.R"),
			[]byte("library(shiny)\nshinyApp(ui, server)"), 0o644)
		os.WriteFile(filepath.Join(badPaths.Unpacked, "DESCRIPTION"),
			[]byte("Package: badapp\nVersion: 0.1.0\nImports:\n    this.package.does.not.exist.12345\n"), 0o644)
		os.WriteFile(filepath.Join(badPaths.Unpacked, "manifest.json"),
			[]byte(`{"version":1,"platform":"4.4.3","metadata":{"appmode":"shiny","entrypoint":"app.R"},"repositories":[{"Name":"CRAN","URL":"https://p3m.dev/cran/latest"}],"description":{"Imports":"this.package.does.not.exist.12345"},"files":{"app.R":{"checksum":"abc"}}}`),
			0o644)

		badApp := &db.AppRow{
			ID:           badAppID,
			ActiveBundle: &badBundleID,
		}

		m, err := manifest.Read(filepath.Join(badPaths.Unpacked, "manifest.json"))
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}

		taskID := uuid.New().String()
		sender := srv.Tasks.Create(taskID, badAppID)
		changed := srv.RunRefresh(context.Background(), badApp, m, sender)

		status, _ := srv.Tasks.Status(taskID)
		if status != task.Failed {
			snap, _, _, _ := srv.Tasks.Subscribe(taskID)
			t.Errorf("expected Failed for nonexistent package, got %d; logs:\n%s",
				status, strings.Join(snap, "\n"))
		}
		if changed {
			t.Error("expected no change when build fails with bad DESCRIPTION")
		}
		t.Log("refresh correctly failed for nonexistent package dependency")
	})
}
