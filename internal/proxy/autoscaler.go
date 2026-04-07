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
//   - Evicts workers that have been idle beyond idle_worker_timeout.
//   - Evicts workers that have crashed (health check fails).
//   - Spawns a new worker if all existing workers are at capacity and
//     the per-app and global limits allow it (eager scale-up).
//   - Maintains pre-warmed worker pools for configured apps.
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

	// Pre-warming: maintain warm pools for all configured apps.
	// This runs after eviction so deficit counts are accurate.
	// Also checks apps that currently have zero workers (not in
	// appIDs above) — they may have pre_warmed_sessions > 0 and need
	// workers spawned from scratch.
	preWarmApps(ctx, srv)

	// App alias lifecycle: transition expired aliases to redirect phase,
	// then clean up expired redirects. These are cheap queries (table
	// is tiny) and this tick is the natural home for lifecycle housekeeping.
	if err := srv.DB.TransitionExpiredAliases(); err != nil {
		slog.Error("autoscaler: alias transition failed", "error", err)
	}
	if err := srv.DB.CleanupExpiredRedirects(); err != nil {
		slog.Error("autoscaler: redirect cleanup failed", "error", err)
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

// preWarmApps checks all apps with pre_warmed_sessions > 0 and spawns
// standby workers to maintain the target pool size. Runs on each
// autoscaler tick as a safety net for the event-driven trigger.
func preWarmApps(ctx context.Context, srv *server.Server) {
	apps, err := srv.DB.ListPreWarmedApps()
	if err != nil {
		slog.Warn("pre-warm: list apps failed", "error", err)
		return
	}
	for _, app := range apps {
		if !app.Enabled || srv.Workers.IsDraining(app.ID) {
			continue
		}
		ensurePreWarmed(ctx, srv, &app)
	}
}

// ensurePreWarmed spawns workers to maintain the pre-warmed pool for an
// app. Called from both the autoscaler tick and the proxy handler (when
// a warm worker is claimed). Respects per-app and global worker limits.
// Spawns are routed through spawnGroup to deduplicate against concurrent
// callers (event-driven trigger vs autoscaler tick) and the loading page
// triggerSpawn path.
//
// Counts free session slots across non-draining workers (rather than
// idle workers) so that with max_sessions_per_worker > 1, a single
// partially-full worker can still satisfy the pre-warm target. Spawns
// whole workers — the allocation unit — until free slots meet the
// target, overshooting by at most max_sessions_per_worker - 1 slots.
func ensurePreWarmed(ctx context.Context, srv *server.Server, app *db.AppRow) {
	if app.PreWarmedSessions <= 0 {
		return
	}

	maxSessions := app.MaxSessionsPerWorker
	if maxSessions < 1 {
		maxSessions = 1
	}

	// Sum free session slots across non-draining workers.
	availableWorkers := srv.Workers.ForAppAvailable(app.ID)
	freeSlots := 0
	for _, wid := range availableWorkers {
		count := srv.Sessions.CountForWorker(wid)
		free := maxSessions - count
		if free > 0 {
			freeSlots += free
		}
	}

	if freeSlots >= app.PreWarmedSessions {
		return
	}

	currentWorkers := len(availableWorkers)
	for freeSlots < app.PreWarmedSessions {
		if app.MaxWorkersPerApp != nil && currentWorkers >= *app.MaxWorkersPerApp {
			slog.Debug("pre-warm: per-app limit reached",
				"app_id", app.ID, "limit", *app.MaxWorkersPerApp)
			break
		}
		if srv.Workers.Count() >= srv.Config.Proxy.MaxWorkers {
			slog.Debug("pre-warm: global limit reached",
				"app_id", app.ID, "limit", srv.Config.Proxy.MaxWorkers)
			break
		}

		slog.Info("pre-warm: spawning standby worker",
			"app_id", app.ID,
			"free_slots", freeSlots,
			"target", app.PreWarmedSessions)
		_, _, err := spawnGroup.do(app.ID, func() (string, string, error) {
			return spawnWorker(ctx, srv, app)
		})
		if err != nil {
			slog.Warn("pre-warm: spawn failed",
				"app_id", app.ID, "error", err)
			break
		}
		currentWorkers++
		freeSlots += maxSessions
	}
}

