package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/server"
)

// RunAutoscaler runs as a background goroutine alongside health polling.
// On each tick it checks every running app and:
//   - Evicts workers that have crashed (health check fails).
//   - Spawns a new worker if all existing workers are at capacity and
//     the per-app and global limits allow it (eager scale-up).
//   - Evicts a worker that has zero sessions when at least one other
//     worker for the same app exists (conservative scale-down).
//   - Sweeps sessions that have been idle longer than the configured TTL.
//
// Blocks until ctx is cancelled.
func RunAutoscaler(ctx context.Context, srv *server.Server) {
	interval := srv.Config.Proxy.HealthInterval.Duration
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			autoscaleTick(ctx, srv)
		}
	}
}

func autoscaleTick(ctx context.Context, srv *server.Server) {
	// Sweep idle sessions first — this ensures stale sessions don't
	// prevent scale-down or inflate capacity counts.
	idleTTL := srv.Config.Proxy.SessionIdleTTL.Duration
	if idleTTL > 0 {
		if n := srv.Sessions.SweepIdle(idleTTL); n > 0 {
			slog.Info("autoscaler: swept idle sessions", "count", n)
		}
	}

	appIDs := srv.Workers.AppIDs()

	for _, appID := range appIDs {
		if srv.Draining.Contains(appID) {
			continue
		}

		app, err := srv.DB.GetApp(appID)
		if err != nil || app == nil {
			continue
		}

		workerIDs := srv.Workers.ForApp(appID)
		if len(workerIDs) == 0 {
			continue
		}

		// Health-check workers and evict crashed ones before scaling decisions.
		workerIDs = evictUnhealthy(ctx, srv, workerIDs)
		if len(workerIDs) == 0 {
			continue
		}

		tryScaleUp(ctx, srv, app, workerIDs)
		tryScaleDown(ctx, srv, app, workerIDs)
	}
}

// evictUnhealthy checks each worker's health and evicts any that have
// crashed. Returns the remaining healthy worker IDs.
func evictUnhealthy(ctx context.Context, srv *server.Server, workerIDs []string) []string {
	healthy := make([]string, 0, len(workerIDs))
	for _, wid := range workerIDs {
		if srv.Backend.HealthCheck(ctx, wid) {
			healthy = append(healthy, wid)
		} else {
			slog.Warn("autoscaler: evicting crashed worker", "worker_id", wid)
			ops.EvictWorker(ctx, srv, wid)
		}
	}
	return healthy
}

// tryScaleUp spawns a new worker when all existing workers are at
// capacity and the per-app / global limits allow it.
func tryScaleUp(ctx context.Context, srv *server.Server, app *db.AppRow, workerIDs []string) {
	maxSessions := app.MaxSessionsPerWorker

	// Check if all workers are at capacity.
	for _, wid := range workerIDs {
		if srv.Sessions.CountForWorker(wid) < maxSessions {
			return // at least one worker has room
		}
	}

	// Per-app limit check.
	if app.MaxWorkersPerApp != nil && len(workerIDs) >= *app.MaxWorkersPerApp {
		return
	}

	// Global limit check.
	if srv.Workers.Count() >= srv.Config.Proxy.MaxWorkers {
		return
	}

	slog.Info("autoscaler: scaling up",
		"app_id", app.ID, "current_workers", len(workerIDs))
	_, _, err := spawnWorker(ctx, srv, app)
	if err != nil {
		slog.Warn("autoscaler: scale-up failed",
			"app_id", app.ID, "error", err)
	}
}

// tryScaleDown evicts a worker that has zero sessions when at least one
// other worker for the same app exists.
func tryScaleDown(ctx context.Context, srv *server.Server, app *db.AppRow, workerIDs []string) {
	if len(workerIDs) <= 1 {
		return // keep at least one worker
	}

	for _, wid := range workerIDs {
		if srv.Sessions.CountForWorker(wid) == 0 {
			slog.Info("autoscaler: scaling down idle worker",
				"app_id", app.ID, "worker_id", wid)
			ops.EvictWorker(ctx, srv, wid)
			return // remove at most one per tick
		}
	}
}
