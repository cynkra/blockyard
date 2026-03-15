package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/server"
)

// RunAutoscaler runs as a background goroutine alongside health polling.
// On each tick it:
//   - Sweeps sessions that have been idle longer than the configured TTL.
//   - Marks workers with zero sessions as idle (sets IdleSince).
//   - Evicts workers that have been idle beyond idle_worker_timeout,
//     keeping at least one worker per app (no scale-to-zero).
//   - Evicts workers that have crashed (health check fails).
//   - Spawns a new worker if all existing workers are at capacity and
//     the per-app and global limits allow it (eager scale-up).
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
	slog.Log(ctx, config.LevelTrace, "autoscaler: tick start",
		"worker_count", srv.Workers.Count())

	// Sweep idle sessions first — this ensures stale sessions don't
	// prevent scale-down or inflate capacity counts.
	idleTTL := srv.Config.Proxy.SessionIdleTTL.Duration
	if idleTTL > 0 {
		if n := srv.Sessions.SweepIdle(idleTTL); n > 0 {
			slog.Info("autoscaler: swept idle sessions", "count", n)
		}
	}

	// Mark workers idle when their last session has been swept.
	// Without this, HTTP-only workers whose sessions were removed above
	// would never get their IdleSince set and would run forever.
	now := time.Now()
	for _, wid := range srv.Workers.All() {
		if srv.Sessions.CountForWorker(wid) == 0 {
			srv.Workers.SetIdleSinceIfZero(wid, now)
		}
	}

	// Evict workers that have been idle beyond the configured timeout.
	idleWorkerTimeout := srv.Config.Proxy.IdleWorkerTimeout.Duration
	idle := srv.Workers.IdleWorkers(idleWorkerTimeout)
	for _, wid := range idle {
		slog.Info("autoscaler: evicting idle worker",
			"worker_id", wid, "idle_for", idleWorkerTimeout)
		ops.EvictWorker(ctx, srv, wid)
	}

	appIDs := srv.Workers.AppIDs()

	for _, appID := range appIDs {
		if srv.Workers.IsDraining(appID) {
			continue
		}

		app, err := srv.DB.GetApp(appID)
		if err != nil || app == nil {
			continue
		}

		workerIDs := srv.Workers.ForAppAvailable(appID)
		if len(workerIDs) == 0 {
			continue
		}

		// Health-check workers and evict crashed ones before scaling decisions.
		workerIDs = evictUnhealthy(ctx, srv, workerIDs)
		if len(workerIDs) == 0 {
			continue
		}

		tryScaleUp(ctx, srv, app, workerIDs)
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
		count := srv.Sessions.CountForWorker(wid)
		slog.Log(ctx, config.LevelTrace, "autoscaler: worker load",
			"app_id", app.ID, "worker_id", wid,
			"sessions", count, "max_sessions", maxSessions)
		if count < maxSessions {
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

