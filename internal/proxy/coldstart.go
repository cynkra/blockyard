package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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
	if srv.Draining.Contains(app.ID) {
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
func spawnWorker(ctx context.Context, srv *server.Server, app *db.AppRow) (workerID, addr string, err error) {
	// Check global worker limit.
	if srv.Workers.Count() >= srv.Config.Proxy.MaxWorkers {
		return "", "", errMaxWorkers
	}

	// Must have an active bundle.
	if app.ActiveBundle == nil {
		return "", "", errNoBundle
	}

	wid := uuid.New().String()
	hostPaths := bundle.NewBundlePaths(
		srv.Config.Storage.BundleServerPath, app.ID, *app.ActiveBundle,
	)

	labels := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    app.ID,
		"dev.blockyard/worker-id": wid,
		"dev.blockyard/role":      "worker",
	}

	var extraEnv map[string]string
	if srv.Config.Openbao != nil {
		extraEnv = map[string]string{
			"VAULT_ADDR":        srv.Config.Openbao.Address,
			"BLOCKYARD_API_URL": srv.Config.Server.ExternalURL,
		}
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
		Env:         extraEnv,
	}

	if err := srv.Backend.Spawn(ctx, spec); err != nil {
		return "", "", fmt.Errorf("spawn worker: %w", err)
	}

	a, err := srv.Backend.Addr(ctx, wid)
	if err != nil {
		srv.Backend.Stop(ctx, wid)
		return "", "", fmt.Errorf("resolve worker address: %w", err)
	}

	srv.Workers.Set(wid, server.ActiveWorker{AppID: app.ID})
	srv.Registry.Set(wid, a)

	// 6. Start log capture before health polling so startup output is captured.
	ops.SpawnLogCapture(context.Background(), srv, wid, app.ID)

	if err := pollHealthy(ctx, srv, wid); err != nil {
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
