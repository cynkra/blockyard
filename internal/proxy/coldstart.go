package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/server"
)

var (
	errMaxWorkers    = errors.New("max workers reached")
	errNoBundle      = errors.New("app has no active bundle")
	errHealthTimeout = errors.New("worker did not become healthy in time")
)

// ensureWorker returns an existing healthy worker for the app, or spawns
// a new one and waits for it to become healthy.
func ensureWorker(ctx context.Context, srv *server.Server, app *db.AppRow) (workerID, addr string, err error) {
	// 1. Check for existing worker
	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) > 0 {
		wid := workerIDs[0]
		a, ok := srv.Registry.Get(wid)
		if ok {
			return wid, a, nil
		}
		// Registry miss — try to re-resolve address
		a, err := srv.Backend.Addr(ctx, wid)
		if err == nil {
			srv.Registry.Set(wid, a)
			return wid, a, nil
		}
		// Worker unreachable — evict stale entry and spawn fresh
		slog.Warn("evicting stale worker", "worker_id", wid, "error", err)
		ops.EvictWorker(ctx, srv, wid)
	}

	// 2. Check global worker limit
	if srv.Workers.Count() >= srv.Config.Proxy.MaxWorkers {
		return "", "", errMaxWorkers
	}

	// 3. Must have an active bundle
	if app.ActiveBundle == nil {
		return "", "", errNoBundle
	}

	// 4. Build WorkerSpec and spawn
	wid := uuid.New().String()
	hostPaths := bundle.NewBundlePaths(
		srv.Config.Storage.DockerBasePath(), app.ID, *app.ActiveBundle,
	)

	labels := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    app.ID,
		"dev.blockyard/worker-id": wid,
		"dev.blockyard/role":      "worker",
	}

	spec := backend.WorkerSpec{
		AppID:       app.ID,
		WorkerID:    wid,
		Image:       srv.Config.Docker.Image,
		BundlePath:  hostPaths.Unpacked,
		LibraryPath: hostPaths.Library,
		WorkerMount: srv.Config.Storage.BundleWorkerPath,
		ShinyPort:   srv.Config.Docker.ShinyPort,
		MemoryLimit: ptrOr(app.MemoryLimit, ""),
		CPULimit:    ptrOr(app.CPULimit, 0.0),
		Labels:      labels,
	}

	if err := srv.Backend.Spawn(ctx, spec); err != nil {
		return "", "", fmt.Errorf("spawn worker: %w", err)
	}

	// 5. Resolve address and register
	a, err := srv.Backend.Addr(ctx, wid)
	if err != nil {
		// Spawn succeeded but can't resolve address — stop and bail
		srv.Backend.Stop(ctx, wid)
		return "", "", fmt.Errorf("resolve worker address: %w", err)
	}

	srv.Workers.Set(wid, server.ActiveWorker{AppID: app.ID})
	srv.Registry.Set(wid, a)

	// 6. Start log capture before health polling so startup output is captured.
	ops.SpawnLogCapture(context.Background(), srv, wid, app.ID)

	// 7. Cold-start hold — poll health with exponential backoff
	if err := pollHealthy(ctx, srv, wid); err != nil {
		// Health check timed out — evict the worker
		srv.Workers.Delete(wid)
		srv.Registry.Delete(wid)
		srv.Backend.Stop(context.Background(), wid)
		return "", "", err
	}

	slog.Info("worker ready",
		"worker_id", wid, "app_id", app.ID, "addr", a)
	return wid, a, nil
}

// pollHealthy polls backend.HealthCheck with exponential backoff until
// the worker is healthy or worker_start_timeout expires.
func pollHealthy(ctx context.Context, srv *server.Server, workerID string) error {
	timeout := srv.Config.Proxy.WorkerStartTimeout.Duration
	deadline := time.Now().Add(timeout)
	interval := 100 * time.Millisecond
	maxInterval := 2 * time.Second

	for {
		if time.Now().After(deadline) {
			return errHealthTimeout
		}

		if srv.Backend.HealthCheck(ctx, workerID) {
			return nil
		}

		// Exponential backoff capped at maxInterval
		select {
		case <-ctx.Done():
			return ctx.Err()
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
