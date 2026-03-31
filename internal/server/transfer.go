package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/pkgstore"
)

// TransferDir returns the host-side transfer directory for a worker.
func (srv *Server) TransferDir(workerID string) string {
	return filepath.Join(srv.Config.Storage.BundleServerPath,
		".transfers", workerID)
}

// handleTransfer is called when a version conflict is detected during
// runtime package install. Copies the store-manifest to the transfer
// directory and starts watching for the board state file.
func (srv *Server) handleTransfer(
	ctx context.Context,
	appID, workerID, storeManifestPath string,
	buildResult *backend.BuildResult,
) (PackageResponse, error) {
	transferDir := srv.TransferDir(workerID)

	// Copy store-manifest to the transfer directory before returning.
	// The staging directory is cleaned up by the caller's defer.
	transferManifest := filepath.Join(transferDir, "store-manifest.json")
	if err := copyFile(storeManifestPath, transferManifest); err != nil {
		return PackageResponse{},
			fmt.Errorf("copy store-manifest to transfer dir: %w", err)
	}

	// Mark this worker as having a transfer in progress.
	srv.SetTransferring(workerID)

	// Start watching for the board state file in a background goroutine.
	go srv.watchTransfer(ctx, appID, workerID, transferManifest, transferDir)

	return PackageResponse{
		Status:       "transfer",
		Message:      "version conflict — container transfer required",
		TransferPath: "/transfer",
	}, nil
}

// watchTransfer polls for the board state file and completes the
// transfer when it appears, or aborts on timeout.
func (srv *Server) watchTransfer(
	ctx context.Context,
	appID, workerID, storeManifestPath, transferDir string,
) {
	defer srv.ClearTransferring(workerID)

	boardPath := filepath.Join(transferDir, "board.json")
	timeout := srv.Config.Proxy.TransferTimeout.Duration
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	pollInterval := 100 * time.Millisecond

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(boardPath); err == nil {
			// Board state written — proceed with transfer.
			srv.completeTransfer(ctx, appID, workerID,
				storeManifestPath, transferDir)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}

	// Timeout — abort transfer. Remove the transfer directory to prevent
	// a stale board.json from being picked up by a subsequent transfer.
	slog.Error("transfer timeout",
		"worker_id", workerID, "app_id", appID)
	os.RemoveAll(transferDir) //nolint:errcheck
}

// completeTransfer assembles a new library, spawns a new worker, reroutes
// sessions, and evicts the old worker.
func (srv *Server) completeTransfer(
	ctx context.Context,
	appID, oldWorkerID, storeManifestPath, transferDir string,
) {
	storeManifest, err := pkgstore.ReadStoreManifest(storeManifestPath)
	if err != nil {
		slog.Error("transfer: read store-manifest", "error", err)
		return
	}

	// Verify the old worker still exists — it may have been evicted
	// between returning "transfer" and board.json appearing.
	oldWorker, ok := srv.Workers.Get(oldWorkerID)
	if !ok {
		slog.Error("transfer: old worker no longer exists",
			"worker_id", oldWorkerID)
		return
	}

	newWorkerID := uuid.New().String()
	newLibDir := srv.PkgStore.WorkerLibDir(newWorkerID)
	missing, err := srv.PkgStore.AssembleLibrary(newLibDir, storeManifest)
	if err != nil {
		slog.Error("transfer: assemble library", "error", err)
		return
	}
	if len(missing) > 0 {
		slog.Error("transfer: missing store entries, aborting",
			"worker_id", newWorkerID, "missing", missing)
		return
	}

	// Spawn new worker with updated library and old worker's
	// transfer dir (containing board.json).
	spec := srv.buildTransferWorkerSpec(appID, newWorkerID, newLibDir, transferDir, oldWorker.BundleID)
	if err := srv.Backend.Spawn(ctx, spec); err != nil {
		slog.Error("transfer: spawn worker", "error", err)
		return
	}

	addr, err := srv.Backend.Addr(ctx, newWorkerID)
	if err != nil {
		slog.Error("transfer: resolve worker address", "error", err)
		return
	}

	// Start token refresher for the new worker.
	var cancelToken func()
	if srv.WorkerTokenKey != nil {
		_, cancelToken, _ = SpawnTokenRefresher(
			context.Background(), srv.Config.Storage.BundleServerPath,
			srv.WorkerTokenKey, appID, newWorkerID)
	}

	srv.Workers.Set(newWorkerID, ActiveWorker{
		AppID: oldWorker.AppID, BundleID: oldWorker.BundleID,
		StartedAt:   time.Now(),
		CancelToken: cancelToken,
	})
	srv.Registry.Set(newWorkerID, addr)

	// Wait for new worker to become healthy.
	if err := srv.waitHealthy(ctx, newWorkerID); err != nil {
		slog.Error("transfer: worker not healthy, cleaning up",
			"worker_id", newWorkerID, "error", err)
		// Clean up the ghost worker.
		if cancelToken != nil {
			cancelToken()
		}
		srv.Workers.Delete(newWorkerID)
		srv.Registry.Delete(newWorkerID)
		srv.Backend.Stop(ctx, newWorkerID) //nolint:errcheck
		return
	}

	// Reroute sessions from old worker to new worker.
	srv.Sessions.RerouteWorker(oldWorkerID, newWorkerID)

	// Evict old worker.
	srv.EvictWorkerFn(ctx, srv, oldWorkerID)

	slog.Info("transfer complete",
		"app_id", appID,
		"old_worker", oldWorkerID,
		"new_worker", newWorkerID)
}

// buildTransferWorkerSpec creates a WorkerSpec for a transfer target worker.
// The old worker's transfer directory is mounted read-only (contains board.json).
func (srv *Server) buildTransferWorkerSpec(
	appID, workerID, libDir, oldTransferDir, bundleID string,
) backend.WorkerSpec {
	spec := srv.defaultWorkerSpec(appID, workerID, libDir, bundleID)

	if oldTransferDir != "" {
		spec.TransferDir = oldTransferDir
		if spec.Env == nil {
			spec.Env = make(map[string]string)
		}
		spec.Env["BLOCKYARD_TRANSFER_PATH"] = "/transfer/board.json"
	}

	return spec
}

// defaultWorkerSpec builds a WorkerSpec with standard settings.
// Used by both container transfer and refresh drain-and-replace.
func (srv *Server) defaultWorkerSpec(
	appID, workerID, libDir, bundleID string,
) backend.WorkerSpec {
	hostPaths := srv.BundlePaths(appID, bundleID)
	transferDir := filepath.Join(srv.Config.Storage.BundleServerPath, ".transfers", workerID)
	_ = os.MkdirAll(transferDir, 0o755)

	return backend.WorkerSpec{
		AppID:    appID,
		WorkerID: workerID,
		Image:    srv.Config.Docker.Image,
		Cmd: []string{"R", "-e",
			fmt.Sprintf("shiny::runApp('%s', port = as.integer(Sys.getenv('SHINY_PORT')))",
				srv.Config.Storage.BundleWorkerPath)},
		BundlePath:  hostPaths.Unpacked,
		LibraryPath: hostPaths.Library,
		LibDir:      libDir,
		TransferDir: transferDir,
		WorkerMount: srv.Config.Storage.BundleWorkerPath,
		ShinyPort:   srv.Config.Docker.ShinyPort,
		Labels: map[string]string{
			"dev.blockyard/managed":   "true",
			"dev.blockyard/app-id":    appID,
			"dev.blockyard/worker-id": workerID,
			"dev.blockyard/role":      "worker",
		},
		Env: WorkerEnv(srv),
	}
}

// waitHealthy polls the backend until the worker is healthy or the
// context expires.
func (srv *Server) waitHealthy(ctx context.Context, workerID string) error {
	timeout := srv.Config.Proxy.WorkerStartTimeout.Duration
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	interval := 100 * time.Millisecond
	maxInterval := 2 * time.Second
	for {
		if srv.Backend.HealthCheck(ctx, workerID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("worker %s did not become healthy", workerID)
		case <-time.After(interval):
		}
		interval = min(interval*2, maxInterval)
	}
}

// copyFile copies src to dst using a temporary file + rename for atomicity.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		if closeErr := out.Close(); closeErr != nil {
			err = fmt.Errorf("%w (close: %w)", err, closeErr)
		}
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// fileExists returns true if the path exists and is a regular file.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// dirExists returns true if the path exists and is a directory.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
