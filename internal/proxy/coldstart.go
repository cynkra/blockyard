package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/mount"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/pkgstore"
	"github.com/cynkra/blockyard/internal/server"
)

var (
	errMaxWorkers    = errors.New("max workers reached")
	errNoBundle      = errors.New("app has no active bundle")
	errHealthTimeout = errors.New("worker did not become healthy in time")
	errAppDraining   = errors.New("app is shutting down")
)

var lb LoadBalancer

// spawnGroup deduplicates concurrent spawn calls per app.
// Key: appID, Value: result from spawnWorker.
var spawnGroup spawnSingleFlight

// spawnSingleFlight provides per-key deduplication for worker spawns.
// Similar to x/sync/singleflight but typed for our use case.
type spawnSingleFlight struct {
	mu    sync.Mutex
	calls map[string]*spawnCall
}

type spawnCall struct {
	wg       sync.WaitGroup
	workerID string
	addr     string
	err      error
}

// do executes fn once per key. Concurrent callers with the same key
// block until the first caller completes and share its result.
func (g *spawnSingleFlight) do(key string, fn func() (string, string, error)) (string, string, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*spawnCall)
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.workerID, c.addr, c.err
	}
	c := &spawnCall{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.workerID, c.addr, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()

	return c.workerID, c.addr, c.err
}

// ensureWorker returns an existing worker with available capacity for
// the app, or spawns a new one and waits for it to become healthy.
// Uses the load balancer to distribute sessions across workers.
// Concurrent calls for the same app are deduplicated — only one spawn
// runs at a time per app.
func ensureWorker(ctx context.Context, srv *server.Server, app *db.AppRow) (workerID, addr string, err error) {
	// Reject new sessions for apps being drained.
	if srv.Workers.IsDraining(app.ID) {
		return "", "", errAppDraining
	}

	// Try to assign to an existing worker with capacity.
	wid, err := lb.Assign(
		app.ID,
		srv.Workers,
		srv.Sessions,
		app.MaxSessionsPerWorker,
		app.MaxWorkersPerApp,
	)
	if err != nil {
		return "", "", err // errCapacityExhausted
	}

	if wid != "" {
		// Assigned to an existing worker — resolve its address.
		slog.Debug("proxy: assigned to existing worker",
			"app_id", app.ID, "worker_id", wid)
		a, ok := srv.Registry.Get(wid)
		if ok {
			return wid, a, nil
		}
		// Registry miss — try to re-resolve.
		a, addrErr := srv.Backend.Addr(ctx, wid)
		if addrErr == nil {
			srv.Registry.Set(wid, a)
			return wid, a, nil
		}
		// Worker unreachable — evict and fall through to spawn.
		slog.Warn("evicting stale worker", "worker_id", wid, "error", addrErr)
		ops.EvictWorker(ctx, srv, wid)
	}

	// No worker with capacity — spawn a new one.
	// Deduplicate concurrent spawns for the same app.
	return spawnGroup.do(app.ID, func() (string, string, error) {
		// Re-check after acquiring the spawn slot — a concurrent request
		// may have just finished spawning a worker with capacity.
		wid, err := lb.Assign(
			app.ID,
			srv.Workers,
			srv.Sessions,
			app.MaxSessionsPerWorker,
			app.MaxWorkersPerApp,
		)
		if err != nil {
			return "", "", err
		}
		if wid != "" {
			if a, ok := srv.Registry.Get(wid); ok {
				return wid, a, nil
			}
		}
		return spawnWorker(ctx, srv, app)
	})
}

// spawnWorker creates a new worker for the app, waits for it to become
// healthy, and registers it. Used by both the proxy cold-start path and
// the autoscaler.
func spawnWorker(ctx context.Context, srv *server.Server, app *db.AppRow) (string, string, error) {
	// Check global worker limit.
	if srv.Workers.Count() >= srv.Config.Proxy.MaxWorkers {
		return "", "", errMaxWorkers
	}

	// Must have an active bundle.
	if app.ActiveBundle == nil {
		return "", "", errNoBundle
	}

	// Use a dedicated context for Docker operations so that a client
	// disconnect (request context cancellation) does not abort container
	// creation mid-flight. The worker_start_timeout bounds the total time.
	timeout := srv.Config.Proxy.WorkerStartTimeout.Duration
	spawnCtx, spawnCancel := context.WithTimeout(context.Background(), timeout)
	defer spawnCancel()

	wid := uuid.New().String()
	slog.Info("spawning worker", //nolint:gosec // G706: slog structured logging handles this
		"worker_id", wid, "app_id", app.ID,
		"bundle_id", *app.ActiveBundle,
		"current_workers", srv.Workers.Count())
	hostPaths := bundle.NewBundlePaths(
		srv.Config.Storage.BundleServerPath, app.ID, *app.ActiveBundle,
	)

	labels := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    app.ID,
		"dev.blockyard/worker-id": wid,
		"dev.blockyard/role":      "worker",
	}

	extraEnv := server.WorkerEnv(srv)

	// Assemble per-worker library from the package store.
	var libDir string
	if srv.PkgStore != nil {
		libDir = srv.PkgStore.WorkerLibDir(wid)

		manifestPath := filepath.Join(hostPaths.Base, "store-manifest.json")
		storeManifest, err := pkgstore.ReadStoreManifest(manifestPath)
		if err != nil {
			// No store-manifest — pre-store bundle. Fall back to
			// legacy library path (LibraryPath).
			slog.Debug("no store-manifest, using legacy library",
				"worker_id", wid, "error", err)
			libDir = ""
		} else {
			missing, err := srv.PkgStore.AssembleLibrary(libDir, storeManifest)
			if err != nil {
				return "", "", fmt.Errorf("assemble library: %w", err)
			}
			if len(missing) > 0 {
				slog.Warn("worker library: missing store entries",
					"worker_id", wid, "missing", missing)
			}
		}
	}

	// Pre-create transfer directory for container transfer signaling.
	// Mounted rw at /transfer inside the container.
	transferDir := filepath.Join(srv.Config.Storage.BundleServerPath, ".transfers", wid)
	if err := os.MkdirAll(transferDir, 0o755); err != nil { //nolint:gosec // G301: transfer dir, not secrets
		slog.Warn("failed to create transfer dir", "worker_id", wid, "error", err)
		transferDir = ""
	}

	// Start token refresher so the worker can authenticate to the
	// packages endpoint. Writes an initial token synchronously.
	var tokDir string
	var cancelToken func()
	if srv.WorkerTokenKey != nil {
		var tokErr error
		tokDir, cancelToken, tokErr = server.SpawnTokenRefresher(
			context.Background(), srv.Config.Storage.BundleServerPath,
			srv.WorkerTokenKey, app.ID, wid)
		if tokErr != nil {
			slog.Warn("failed to start token refresher",
				"worker_id", wid, "error", tokErr)
		}
	}

	// cleanupLocal releases resources created in this function that are
	// not yet tracked by EvictWorker (token refresher, dirs, pkg lib).
	cleanupLocal := func() {
		if cancelToken != nil {
			cancelToken()
		}
		if transferDir != "" {
			_ = os.RemoveAll(transferDir)
		}
		if tokDir != "" {
			_ = os.RemoveAll(tokDir) //nolint:gosec // G703: server-created temp dir cleanup
		}
		if libDir != "" && srv.PkgStore != nil {
			_ = srv.PkgStore.CleanupWorkerLib(wid)
		}
	}

	// Read R version from bundle manifest for version dispatch.
	var rVersion string
	if m, err := manifest.Read(filepath.Join(hostPaths.Unpacked, "manifest.json")); err == nil {
		rVersion = m.RVersion
	}

	spec := backend.WorkerSpec{
		AppID:       app.ID,
		WorkerID:    wid,
		Image:       server.AppImage(app, srv.Config.Docker.Image),
		Cmd: []string{"R", "-e",
			fmt.Sprintf("shiny::runApp('%s', port = as.integer(Sys.getenv('SHINY_PORT')), host = Sys.getenv('SHINY_HOST', unset = '0.0.0.0'))",
				srv.Config.Storage.BundleWorkerPath)},
		BundlePath:  hostPaths.Unpacked,
		LibraryPath: hostPaths.Library,
		LibDir:      libDir,
		TransferDir: transferDir,
		TokenDir:    tokDir,
		WorkerMount: srv.Config.Storage.BundleWorkerPath,
		ShinyPort:   srv.Config.Docker.ShinyPort,
		RVersion:    rVersion,
		MemoryLimit: ptrOr(app.MemoryLimit, ""),
		CPULimit:    ptrOr(app.CPULimit, 0.0),
		Labels:      labels,
		Env:         extraEnv,
		Runtime:     server.AppRuntime(app, srv.Config.Docker),
	}

	// Resolve per-app data mounts.
	appMounts, err := srv.DB.ListAppDataMounts(app.ID)
	if err != nil {
		slog.Error("failed to list data mounts", "app", app.Name, "error", err) //nolint:gosec // G706: slog structured logging handles this
	} else if len(appMounts) > 0 {
		resolved, err := mount.Resolve(appMounts, srv.Config.Storage.DataMounts)
		if err != nil {
			slog.Error("failed to resolve data mounts", "app", app.Name, "error", err) //nolint:gosec // G706: slog structured logging handles this
		} else {
			spec.DataMounts = resolved
		}
	}

	if err := srv.Backend.Spawn(spawnCtx, spec); err != nil {
		cleanupLocal()
		return "", "", fmt.Errorf("spawn worker: %w", err)
	}

	a, err := srv.Backend.Addr(spawnCtx, wid)
	if err != nil {
		srv.Backend.Stop(spawnCtx, wid) //nolint:errcheck // best-effort cleanup
		cleanupLocal()
		return "", "", fmt.Errorf("resolve worker address: %w", err)
	}

	srv.Workers.Set(wid, server.ActiveWorker{
		AppID: app.ID, BundleID: *app.ActiveBundle,
		StartedAt: time.Now(),
	})
	srv.SetCancelToken(wid, cancelToken)
	srv.Registry.Set(wid, a)

	// 6. Start log capture before health polling so startup output is captured.
	ops.SpawnLogCapture(context.Background(), srv, wid, app.ID)

	coldStartBegin := time.Now()
	if err := pollHealthy(spawnCtx, srv, wid); err != nil {
		slog.Warn("worker failed health check during cold start", //nolint:gosec // G706: slog structured logging handles this
			"worker_id", wid, "app_id", app.ID,
			"elapsed", time.Since(coldStartBegin).Round(time.Millisecond),
			"error", err)
		srv.CancelTokenRefresher(wid)
		srv.Workers.Delete(wid)
		srv.Registry.Delete(wid)
		srv.Backend.Stop(context.Background(), wid) //nolint:errcheck // best-effort cleanup
		cleanupLocal()
		return "", "", err
	}
	coldStartElapsed := time.Since(coldStartBegin)
	srv.Metrics.ColdStartDuration.Observe(coldStartElapsed.Seconds())
	srv.Metrics.WorkersSpawned.Inc()
	srv.Metrics.WorkersActive.Inc()

	slog.Info("worker ready", //nolint:gosec // G706: slog structured logging handles this
		"worker_id", wid, "app_id", app.ID, "addr", a,
		"cold_start_ms", coldStartElapsed.Milliseconds())
	return wid, a, nil
}

// pollHealthy polls backend.HealthCheck with exponential backoff until
// the worker is healthy or the context expires (worker_start_timeout).
func pollHealthy(ctx context.Context, srv *server.Server, workerID string) error {
	interval := 100 * time.Millisecond
	maxInterval := 2 * time.Second

	for {
		if srv.Backend.HealthCheck(ctx, workerID) {
			return nil
		}

		select {
		case <-ctx.Done():
			return errHealthTimeout
		case <-time.After(interval):
		}
		interval = min(interval*2, maxInterval)
	}
}


func ptrOr[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}

// hasAvailableWorker returns true if the app has at least one available
// (non-draining) worker registered in the worker map.
func hasAvailableWorker(srv *server.Server, appID string) bool {
	return len(srv.Workers.ForAppAvailable(appID)) > 0
}

// triggerSpawn spawns a worker for the app in the background. Uses
// spawnGroup to deduplicate concurrent calls. Errors are logged
// but not returned — the loading page polls for readiness.
func triggerSpawn(srv *server.Server, app *db.AppRow) {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		srv.Config.Proxy.WorkerStartTimeout.Duration,
	)
	defer cancel()

	_, _, err := spawnGroup.do(app.ID, func() (string, string, error) {
		return spawnWorker(ctx, srv, app)
	})
	if err != nil {
		slog.Warn("triggerSpawn: background spawn failed",
			"app_id", app.ID, "error", err)
	}
}
