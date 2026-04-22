package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/telemetry"
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

	// Mark workers idle when their last WebSocket has gone. A session
	// is one active WS, so zero WS means nobody is currently using the
	// worker — cookie pins alone don't keep it alive.
	now := time.Now()
	for _, wid := range srv.Workers.All() {
		if srv.WsConns.Count(wid) == 0 {
			srv.Workers.SetIdleSinceIfZero(wid, now)
		}
	}

	// Evict workers that have been idle beyond the configured timeout.
	idleWorkerTimeout := srv.Config.Proxy.IdleWorkerTimeout.Duration
	idle := srv.Workers.IdleWorkers(idleWorkerTimeout)
	for _, wid := range idle {
		slog.Info("autoscaler: evicting idle worker",
			"worker_id", wid, "idle_for", idleWorkerTimeout)
		ops.EvictWorker(ctx, srv, wid, telemetry.ReasonIdleTimeout)
	}

	appIDs := srv.Workers.AppIDs()

	for _, appID := range appIDs {
		app, err := srv.DB.GetApp(appID)
		if err != nil || app == nil {
			continue
		}
		if !app.Enabled {
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

	// Refresh the workers-by-state gauge from the (now reconciled)
	// worker map. Doing this last ensures the snapshot reflects
	// evictions and idle marks applied earlier in the tick.
	reconcileWorkersByState(srv)
}

// reconcileWorkersByState sets blockyard_workers{state=…} to the
// current count of workers in each state. Called from the autoscaler
// tick; derives state from ActiveWorker.Draining and per-worker
// session counts rather than tracking explicit transitions.
func reconcileWorkersByState(srv *server.Server) {
	var busy, idle, draining float64
	for _, wid := range srv.Workers.All() {
		w, ok := srv.Workers.Get(wid)
		if !ok {
			continue
		}
		switch {
		case w.Draining:
			draining++
		case srv.WsConns.Count(wid) > 0:
			busy++
		default:
			idle++
		}
	}
	srv.Metrics.WorkersByState.WithLabelValues(telemetry.StateBusy).Set(busy)
	srv.Metrics.WorkersByState.WithLabelValues(telemetry.StateIdle).Set(idle)
	srv.Metrics.WorkersByState.WithLabelValues(telemetry.StateDraining).Set(draining)
}

// evictUnhealthy checks each worker's health and evicts any that have
// crashed. Returns the remaining healthy worker IDs. Workers still
// within their cold-start window are left alone — spawnWorker's
// pollHealthy owns their lifecycle until it returns.
func evictUnhealthy(ctx context.Context, srv *server.Server, workerIDs []string) []string {
	healthy := make([]string, 0, len(workerIDs))
	coldStart := srv.Config.Proxy.WorkerStartTimeout.Duration
	now := time.Now()
	for _, wid := range workerIDs {
		if srv.Backend.HealthCheck(ctx, wid) {
			healthy = append(healthy, wid)
			continue
		}
		if w, ok := srv.Workers.Get(wid); ok && now.Sub(w.StartedAt) < coldStart {
			healthy = append(healthy, wid)
			continue
		}
		slog.Warn("autoscaler: evicting crashed worker", "worker_id", wid)
		ops.EvictWorker(ctx, srv, wid, telemetry.ReasonCrashed)
	}
	return healthy
}

// tryScaleUp spawns a new worker when all existing workers are at
// capacity and the per-app / global limits allow it.
func tryScaleUp(ctx context.Context, srv *server.Server, app *db.AppRow, workerIDs []string) {
	maxSessions := app.MaxSessionsPerWorker

	// Check if all workers are at capacity. Load is active WebSocket
	// count — that's what max_sessions_per_worker gates.
	for _, wid := range workerIDs {
		count := srv.WsConns.Count(wid)
		slog.Log(ctx, config.LevelTrace, "autoscaler: worker load",
			"app_id", app.ID, "worker_id", wid,
			"ws", count, "max_sessions", maxSessions)
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
		if !app.Enabled {
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

	// Sum free session slots across non-draining workers. A slot is
	// "free" when no WebSocket is currently occupying it.
	availableWorkers := srv.Workers.ForAppAvailable(app.ID)
	freeSlots := 0
	for _, wid := range availableWorkers {
		count := srv.WsConns.Count(wid)
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

