package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/buildercache"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/pakcache"
	"github.com/cynkra/blockyard/internal/pkgstore"
	"github.com/cynkra/blockyard/internal/task"
)

// RunRefresh re-resolves dependencies for an unpinned deployment.
// Returns true if a new worker was spawned (dependencies changed).
func (srv *Server) RunRefresh(
	ctx context.Context,
	app *db.AppRow,
	m *manifest.Manifest,
	sender task.Sender,
) bool {
	status := task.Completed
	defer func() { sender.Complete(status) }()

	sender.Write("Refreshing dependencies...")

	bsp := srv.Config.Storage.BundleServerPath

	// 1. Ensure pak and by-builder are cached.
	pakPath, err := pakcache.EnsureInstalled(
		ctx, srv.Backend, AppImage(app, srv.Config.Docker.Image),
		srv.Config.Docker.PakVersion,
		filepath.Join(bsp, ".pak-cache"))
	if err != nil {
		sender.Write("Failed to set up build tools.")
		slog.Error("refresh: ensure pak", "error", err)
		status = task.Failed
		return false
	}
	builderPath, err := buildercache.EnsureCached(
		filepath.Join(bsp, ".by-builder-cache"), srv.Version)
	if err != nil {
		sender.Write("Failed to set up build tools.")
		slog.Error("refresh: ensure by-builder", "error", err)
		status = task.Failed
		return false
	}

	// 2. Get the bundle's unpacked path (contains DESCRIPTION / scripts).
	bundlePaths := srv.BundlePaths(app.ID, *app.ActiveBundle)

	// 3. Run the standard build flow using the original unpinned manifest.
	buildUUID := uuid.New().String()
	dlCachePath := filepath.Join(bsp, ".pak-dl-cache")
	os.MkdirAll(dlCachePath, 0o755) //nolint:errcheck,gosec // G301: download cache dir, not secrets

	spec := backend.BuildSpec{
		AppID:    app.ID,
		BundleID: "refresh-" + buildUUID[:8],
		Image:    AppImage(app, srv.Config.Docker.Image),
		Cmd:      bundle.BuildCommand(),
		Mounts: bundle.BuildMounts(
			pakPath, bundlePaths.Unpacked,
			srv.PkgStore.Root(), dlCachePath, builderPath),
		Env: []string{"BUILD_UUID=" + buildUUID},
		Labels: map[string]string{
			"dev.blockyard/managed": "true",
			"dev.blockyard/role":    "refresh",
			"dev.blockyard/app-id":  app.ID,
		},
		LogWriter: func(line string) { sender.Write(line) },
	}

	result, err := srv.Backend.Build(ctx, spec)
	if err != nil {
		sender.Write("Dependency resolution failed.")
		slog.Error("refresh: build", "error", err)
		status = task.Failed
		return false
	}
	if !result.Success {
		sender.Write(fmt.Sprintf("Dependency resolution failed (exit %d).", result.ExitCode))
		status = task.Failed
		return false
	}

	// 4. Extract store-manifest (primary) and pak.lock (audit) from build dir.
	buildDir := filepath.Join(srv.PkgStore.Root(), ".builds", buildUUID)
	defer os.RemoveAll(buildDir) //nolint:errcheck

	newManifestSrc := filepath.Join(buildDir, "store-manifest.json")
	newManifestDst := filepath.Join(bundlePaths.Base, "store-manifest.json")

	// Also persist pak.lock as a debug/audit artifact.
	newLockfileSrc := filepath.Join(buildDir, "pak.lock")
	newLockfileDst := filepath.Join(bundlePaths.Base, "pak.lock")
	if fileExists(newLockfileSrc) {
		copyFile(newLockfileSrc, newLockfileDst) //nolint:errcheck
	}

	// 5. Archive current store-manifest as .prev for one-step rollback.
	prevManifest := filepath.Join(bundlePaths.Base, "store-manifest.json.prev")
	if fileExists(newManifestDst) {
		copyFile(newManifestDst, prevManifest) //nolint:errcheck
	}

	if err := copyFile(newManifestSrc, newManifestDst); err != nil {
		sender.Write("Failed to save updated dependency manifest.")
		slog.Error("refresh: persist store-manifest", "error", err)
		status = task.Failed
		return false
	}

	// 6. Check if anything actually changed.
	changed, err := storeManifestsChanged(prevManifest, newManifestDst)
	if err != nil {
		slog.Warn("refresh: store-manifest comparison failed, assuming changed",
			"error", err)
		changed = true
	}
	if !changed {
		sender.Write("Dependencies unchanged — no action needed.")
		return false
	}

	// 7. Graceful drain: spawn new worker, drain old ones.
	sender.Write("Dependencies updated — spawning new worker...")
	if err := srv.drainAndReplace(ctx, app, newManifestDst, sender); err != nil {
		slog.Error("refresh: drain-and-replace failed", "app_id", app.ID, "error", err)
		status = task.Failed
	}
	return true
}

// drainAndReplace spawns a new worker with the updated library, marks
// old workers as draining, and lets existing sessions finish.
func (srv *Server) drainAndReplace(
	ctx context.Context,
	app *db.AppRow,
	storeManifestPath string,
	sender task.Sender,
) error {
	storeManifest, err := pkgstore.ReadStoreManifest(storeManifestPath)
	if err != nil {
		sender.Write("Failed to read dependency manifest.")
		return fmt.Errorf("read store-manifest: %w", err)
	}

	// 1. Spawn a new worker with the updated library.
	newWorkerID := uuid.New().String()
	newLibDir := srv.PkgStore.WorkerLibDir(newWorkerID)
	missing, err := srv.PkgStore.AssembleLibrary(newLibDir, storeManifest)
	if err != nil {
		sender.Write("Failed to prepare package library.")
		return fmt.Errorf("assemble library: %w", err)
	}
	if len(missing) > 0 {
		sender.Write(fmt.Sprintf("Warning: %d packages missing from cache.", len(missing)))
	}

	// Mark old workers as draining BEFORE spawning the new one,
	// so ForAppAvailable excludes them immediately.
	oldWorkers := srv.Workers.ForApp(app.ID)
	for _, oldID := range oldWorkers {
		srv.Workers.SetDraining(oldID)
		sender.Write(fmt.Sprintf("Stopping previous worker (%s)...", oldID[:8]))
	}

	// restoreOld undoes draining if the replacement fails.
	// Stale packages are better than no workers at all.
	restoreOld := func() {
		for _, oldID := range oldWorkers {
			srv.Workers.ClearDraining(oldID)
		}
	}

	spec := srv.defaultWorkerSpec(app, newWorkerID, newLibDir, *app.ActiveBundle)
	if err := srv.Backend.Spawn(ctx, spec); err != nil {
		sender.Write("Failed to start new worker.")
		restoreOld()
		return fmt.Errorf("spawn worker: %w", err)
	}

	addr, err := srv.Backend.Addr(ctx, newWorkerID)
	if err != nil {
		sender.Write("Failed to start new worker.")
		srv.Backend.Stop(ctx, newWorkerID) //nolint:errcheck
		restoreOld()
		return fmt.Errorf("resolve worker address: %w", err)
	}

	// Start token refresher for the new worker.
	var cancelToken func()
	if srv.WorkerTokenKey != nil {
		var tokenErr error
		_, cancelToken, tokenErr = SpawnTokenRefresher(
			context.Background(), srv.Config.Storage.BundleServerPath,
			srv.WorkerTokenKey, app.ID, newWorkerID)
		if tokenErr != nil {
			slog.Error("refresh: failed to start token refresher",
				"worker_id", newWorkerID, "error", tokenErr)
		}
	}

	srv.Workers.Set(newWorkerID, ActiveWorker{
		AppID: app.ID, BundleID: *app.ActiveBundle,
		StartedAt: time.Now(),
	})
	srv.SetCancelToken(newWorkerID, cancelToken)
	srv.Registry.Set(newWorkerID, addr)

	// Start log capture for the new worker.
	if srv.SpawnLogCaptureFn != nil {
		srv.SpawnLogCaptureFn(context.Background(), srv, newWorkerID, app.ID)
	}

	if err := srv.waitHealthy(ctx, newWorkerID); err != nil {
		sender.Write(fmt.Sprintf("New worker (%s) failed health check.", newWorkerID[:8]))
		srv.CancelTokenRefresher(newWorkerID)
		srv.Workers.Delete(newWorkerID)
		srv.Registry.Delete(newWorkerID)
		srv.Backend.Stop(ctx, newWorkerID) //nolint:errcheck
		restoreOld()
		return fmt.Errorf("worker health check: %w", err)
	}

	sender.Write(fmt.Sprintf("New worker (%s) ready.", newWorkerID[:8]))
	return nil
}

// storeManifestsChanged compares two store-manifest files.
func storeManifestsChanged(oldPath, newPath string) (bool, error) {
	oldManifest, err := pkgstore.ReadStoreManifest(oldPath)
	if err != nil {
		return false, err
	}
	newManifest, err := pkgstore.ReadStoreManifest(newPath)
	if err != nil {
		return false, err
	}
	if len(oldManifest) != len(newManifest) {
		return true, nil
	}
	for pkg, ref := range newManifest {
		if oldManifest[pkg] != ref {
			return true, nil
		}
	}
	return false, nil
}

// RunRollback performs a rollback to either the previous refresh or the
// original build, then drains and replaces workers.
func (srv *Server) RunRollback(
	ctx context.Context,
	app *db.AppRow,
	target string, // "build" or "" (previous refresh)
	sender task.Sender,
) {
	status := task.Completed
	defer func() { sender.Complete(status) }()

	bundlePaths := srv.BundlePaths(app.ID, *app.ActiveBundle)
	currentManifest := filepath.Join(bundlePaths.Base, "store-manifest.json")

	switch target {
	case "build":
		sender.Write("Rolling back dependencies to original build...")
		buildManifest := filepath.Join(bundlePaths.Base, "store-manifest.json.build")
		if err := copyFile(buildManifest, currentManifest); err != nil {
			sender.Write("Failed to restore original dependencies.")
			slog.Error("refresh rollback: restore build manifest", "error", err)
			status = task.Failed
			return
		}
		// Remove .prev — it's no longer relevant.
		os.Remove(filepath.Join(bundlePaths.Base, "store-manifest.json.prev")) //nolint:errcheck

	default:
		sender.Write("Rolling back dependencies to previous refresh...")
		prevManifest := filepath.Join(bundlePaths.Base, "store-manifest.json.prev")
		// Promote prev to current, discard the bad manifest.
		if err := os.Rename(prevManifest, currentManifest); err != nil {
			sender.Write("Failed to restore previous dependencies.")
			slog.Error("refresh rollback: promote prev manifest", "error", err)
			status = task.Failed
			return
		}
	}

	// Reassemble workers with the rolled-back store-manifest.
	if err := srv.drainAndReplace(ctx, app, currentManifest, sender); err != nil {
		slog.Error("rollback: drain-and-replace failed", "app_id", app.ID, "error", err)
		status = task.Failed
	}
}
