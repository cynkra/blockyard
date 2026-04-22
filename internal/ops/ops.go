package ops

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/telemetry"
)

// EvictWorker is the single codepath for decommissioning a worker.
// Idempotent — safe to call concurrently from multiple goroutines.
// reason is recorded in the blockyard_workers_stopped_total metric
// and must be one of the telemetry.Reason* constants.
func EvictWorker(ctx context.Context, srv *server.Server, workerID, reason string) {
	w, found := srv.Workers.Get(workerID)
	srv.Workers.Delete(workerID)
	if found {
		// Cancel the token refresher goroutine.
		srv.CancelTokenRefresher(workerID)
		slog.Info("evicting worker",
			"worker_id", workerID, "app_id", w.AppID, "reason", reason)
		if err := srv.Backend.Stop(ctx, workerID); err != nil {
			slog.Warn("evict: failed to stop worker",
				"worker_id", workerID, "error", err)
		}
		srv.Metrics.WorkersStopped.WithLabelValues(reason).Inc()
		srv.Metrics.WorkersActive.Dec()
	}
	// End session records in the database.
	if err := srv.DB.EndWorkerSessions(workerID); err != nil {
		slog.Warn("evict: failed to end worker sessions",
			"worker_id", workerID, "error", err)
	}

	srv.Registry.Delete(workerID)
	srv.Sessions.DeleteByWorker(workerID)
	srv.WsConns.DeleteWorker(workerID)
	srv.LogStore.MarkEnded(workerID)
	// SessionsActive is kept in sync by shuttleWS's TryInc/Dec; the
	// shuttles backed by this worker will exit when the backend closes,
	// decrementing the gauge on their own.

	// Clean up worker library from the package store.
	if srv.PkgStore != nil {
		if err := srv.PkgStore.CleanupWorkerLib(workerID); err != nil {
			slog.Warn("evict: failed to clean worker lib",
				"worker_id", workerID, "error", err)
		}
	}
	// Clean up transfer directory.
	transferDir := filepath.Join(srv.Config.Storage.BundleServerPath, ".transfers", workerID)
	_ = os.RemoveAll(transferDir)

	// Clean up worker token directory.
	tokenDir := filepath.Join(srv.Config.Storage.BundleServerPath, ".worker-tokens", workerID)
	_ = os.RemoveAll(tokenDir)

	// Clean up per-worker install mutex.
	srv.CleanupInstallMu(workerID)
}

// StartupCleanup removes orphaned resources and fails stale builds.
// Called in main() before binding the listener. When passive is true
// (rolling update), destructive operations that would kill workers the
// old server is handing off are skipped.
func StartupCleanup(ctx context.Context, srv *server.Server, passive bool) error {
	// In passive mode, skip destructive operations that would kill
	// workers the old server is handing off.
	if !passive {
		// Remove backend-specific orphaned state (Docker: iptables rules).
		if err := srv.Backend.CleanupOrphanResources(ctx); err != nil {
			slog.Warn("startup: orphan resource cleanup failed", "error", err)
		}
	}

	// Clean up orphaned staging directories from previous runs.
	if srv.PkgStore != nil {
		stagingDir := filepath.Join(srv.PkgStore.Root(), ".staging")
		entries, _ := os.ReadDir(stagingDir)
		for _, e := range entries {
			if e.IsDir() {
				os.RemoveAll(filepath.Join(stagingDir, e.Name())) //nolint:errcheck
			}
		}
	}

	// Clean up orphaned transfer directories from previous runs.
	transferBaseDir := filepath.Join(srv.Config.Storage.BundleServerPath, ".transfers")
	transferEntries, _ := os.ReadDir(transferBaseDir)
	for _, e := range transferEntries {
		if e.IsDir() {
			os.RemoveAll(filepath.Join(transferBaseDir, e.Name())) //nolint:errcheck
		}
	}

	if !passive {
		// Worker token directories — bind-mounted into surviving
		// containers; removing them breaks worker→server auth.
		tokenBaseDir := filepath.Join(srv.Config.Storage.BundleServerPath, ".worker-tokens")
		entries, _ := os.ReadDir(tokenBaseDir)
		for _, e := range entries {
			if e.IsDir() {
				os.RemoveAll(filepath.Join(tokenBaseDir, e.Name())) //nolint:errcheck
			}
		}

		// Container force-removal — the new server adopts existing
		// workers via Redis instead.
		resources, err := srv.Backend.ListManaged(ctx)
		if err != nil {
			return err
		}
		if len(resources) > 0 {
			slog.Info("startup: removing orphaned resources",
				"count", len(resources))
		}
		for _, r := range resources {
			if err := srv.Backend.RemoveResource(ctx, r); err != nil {
				slog.Warn("startup: failed to remove orphan",
					"id", r.ID, "error", err)
			}
		}
	}

	count, err := srv.DB.FailStaleBuilds()
	if err != nil {
		return fmt.Errorf("fail stale builds: %w", err)
	}
	if count > 0 {
		slog.Info("startup: marked stale bundles as failed",
			"count", count)
	}

	// Reconcile Redis worker map against containers still running.
	// With in-memory stores this is a no-op (All() returns empty).
	workerIDs := srv.Workers.All()
	if len(workerIDs) > 0 {
		remaining, _ := srv.Backend.ListManaged(ctx)
		alive := make(map[string]bool, len(remaining))
		for _, r := range remaining {
			if wid := r.Labels["dev.blockyard/worker-id"]; wid != "" {
				alive[wid] = true
			}
		}
		var stale int
		for _, wid := range workerIDs {
			if !alive[wid] {
				srv.Workers.Delete(wid)
				srv.Sessions.DeleteByWorker(wid)
				srv.Registry.Delete(wid)
				stale++
			}
		}
		if stale > 0 {
			slog.Info("startup: removed stale worker entries from redis",
				"count", stale)
		}
	}

	return nil
}

const maxMisses = 2

// appLabelForWorker returns the app name for the given worker, or
// telemetry.AppUnknown if the worker/app cannot be resolved. Used for
// labelling per-worker metrics (health-check failures) with a human
// readable app name rather than an opaque ID.
func appLabelForWorker(srv *server.Server, workerID string) string {
	w, ok := srv.Workers.Get(workerID)
	if !ok {
		return telemetry.AppUnknown
	}
	app, err := srv.DB.GetApp(w.AppID)
	if err != nil || app == nil {
		return telemetry.AppUnknown
	}
	return app.Name
}

// evictDrainedWorkers checks draining workers and evicts those with
// zero active WebSockets. Called from the health poller tick.
func evictDrainedWorkers(ctx context.Context, srv *server.Server) {
	for _, wid := range srv.Workers.All() {
		w, ok := srv.Workers.Get(wid)
		if !ok || !w.Draining {
			continue
		}
		if srv.WsConns.Count(wid) == 0 {
			slog.Info("evicting drained worker with zero ws",
				"worker_id", wid, "app_id", w.AppID)
			EvictWorker(ctx, srv, wid, telemetry.ReasonGraceful)
		}
	}
}

func pollOnce(ctx context.Context, srv *server.Server, misses map[string]int) {
	workerIDs := srv.Workers.All()
	if len(workerIDs) == 0 {
		return
	}

	type result struct {
		workerID string
		healthy  bool
	}

	results := make(chan result, len(workerIDs))
	var wg sync.WaitGroup

	for _, wid := range workerIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			healthy := srv.Backend.HealthCheck(ctx, id)
			results <- result{workerID: id, healthy: healthy}
		}(wid)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	active := make(map[string]bool, len(workerIDs))
	for r := range results {
		active[r.workerID] = true
		if r.healthy {
			// Refresh registry TTL on successful health check.
			if addr, ok := srv.Registry.Get(r.workerID); ok {
				srv.Registry.Set(r.workerID, addr)
			}
			delete(misses, r.workerID)
			continue
		}
		// Workers still in their cold-start window are owned by
		// spawnWorker's pollHealthy; don't count misses against them.
		if w, ok := srv.Workers.Get(r.workerID); ok {
			if time.Since(w.StartedAt) < srv.Config.Proxy.WorkerStartTimeout.Duration {
				continue
			}
		}
		misses[r.workerID]++
		if misses[r.workerID] >= maxMisses {
			slog.Warn("health poller: evicting unhealthy worker",
				"worker_id", r.workerID,
				"consecutive_misses", misses[r.workerID])
			srv.Metrics.HealthChecksFailed.WithLabelValues(appLabelForWorker(srv, r.workerID)).Inc()
			// Mark sessions as crashed before eviction (which marks them as ended).
			if err := srv.DB.CrashWorkerSessions(r.workerID); err != nil {
				slog.Warn("health poller: failed to crash worker sessions",
					"worker_id", r.workerID, "error", err)
			}
			EvictWorker(ctx, srv, r.workerID, telemetry.ReasonCrashed)
			delete(misses, r.workerID)
		}
	}

	// Prune miss counts for workers no longer in the snapshot
	for wid := range misses {
		if !active[wid] {
			delete(misses, wid)
		}
	}
}

// SpawnHealthPoller runs health checks at config.Proxy.HealthInterval.
// Blocks until ctx is cancelled.
func SpawnHealthPoller(ctx context.Context, srv *server.Server) {
	interval := srv.Config.Proxy.HealthInterval.Duration
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	misses := make(map[string]int)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pollOnce(ctx, srv, misses)
			evictDrainedWorkers(ctx, srv)
			if srv.VaultTokenCache != nil {
				srv.VaultTokenCache.Sweep()
			}
		}
	}
}

// SpawnLogCapture starts a goroutine that streams logs from the backend
// into the LogStore for the given worker.
func SpawnLogCapture(
	ctx context.Context,
	srv *server.Server,
	workerID, appID string,
) {
	sender := srv.LogStore.Create(workerID, appID)

	go func() {
		stream, err := srv.Backend.Logs(ctx, workerID)
		if err != nil {
			slog.Warn("log capture: failed to open stream",
				"worker_id", workerID, "error", err)
			srv.LogStore.MarkEnded(workerID)
			return
		}
		defer stream.Close()

		for line := range stream.Lines {
			sender.Write(line)
		}
		srv.LogStore.MarkEnded(workerID)
	}()
}

// drainAndEvictAll marks workers as draining, waits for sessions to end,
// then force-evicts all workers. Used during server shutdown.
func drainAndEvictAll(ctx context.Context, srv *server.Server, workerIDs []string) {
	slog.Info("shutdown: draining workers", "count", len(workerIDs))

	// Mark all workers as draining so no new sessions are routed.
	appsSeen := make(map[string]bool)
	for _, wid := range workerIDs {
		w, ok := srv.Workers.Get(wid)
		if ok && !appsSeen[w.AppID] {
			appsSeen[w.AppID] = true
			slog.Debug("shutdown: marking app as draining", "app_id", w.AppID)
			srv.Workers.MarkDraining(w.AppID)
		}
	}

	// Wait for sessions (active WebSockets) to end (up to half of shutdown timeout).
	drainTimeout := srv.Config.Server.ShutdownTimeout.Duration / 2
	deadline := time.Now().Add(drainTimeout)
	for time.Now().Before(deadline) {
		total := srv.WsConns.CountForWorkers(workerIDs)
		if total == 0 {
			slog.Info("shutdown: all sessions drained")
			break
		}
		slog.Debug("shutdown: waiting for sessions to drain", "remaining", total)
		time.Sleep(time.Second)
	}

	// Force-evict all remaining workers.
	var wg sync.WaitGroup
	for _, wid := range workerIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			evictCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			EvictWorker(evictCtx, srv, id, telemetry.ReasonGraceful)
		}(wid)
	}
	wg.Wait()
}

// GracefulShutdown stops all workers, removes managed resources, and
// fails in-progress builds. Called after HTTP server drain and background
// goroutine cancellation.
func GracefulShutdown(ctx context.Context, srv *server.Server) {
	workerIDs := srv.Workers.All()
	if len(workerIDs) > 0 {
		drainAndEvictAll(ctx, srv, workerIDs)
	}

	// Remove remaining managed resources (build containers, networks)
	resources, err := srv.Backend.ListManaged(ctx)
	if err == nil {
		for _, r := range resources {
			_ = srv.Backend.RemoveResource(ctx, r)
		}
	}

	// Fail in-progress builds
	count, err := srv.DB.FailStaleBuilds()
	if err == nil && count > 0 {
		slog.Info("shutdown: marked stale bundles as failed",
			"count", count)
	}
}

// StopAppSync stops all workers for an app synchronously.
// Marks workers as draining, waits for sessions to end (up to
// shutdown_timeout), then force-evicts. If no workers are running,
// this is a no-op.
func StopAppSync(srv *server.Server, appID string) {
	workerIDs := srv.Workers.MarkDraining(appID)
	if len(workerIDs) == 0 {
		return
	}

	deadline := time.Now().Add(srv.Config.Server.ShutdownTimeout.Duration)
	for {
		remaining := srv.WsConns.CountForWorkers(workerIDs)
		if remaining == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}

	for _, wid := range workerIDs {
		EvictWorker(context.Background(), srv, wid, telemetry.ReasonGraceful)
	}
}

// SpawnSoftDeleteSweeper periodically purges soft-deleted apps whose
// retention period has expired. Blocks until ctx is cancelled.
// Does not start if soft_delete_retention is zero (soft-delete
// disabled — nothing to sweep).
func SpawnSoftDeleteSweeper(ctx context.Context, srv *server.Server) {
	retention := srv.Config.Storage.SoftDeleteRetention.Duration
	if retention == 0 {
		<-ctx.Done()
		return
	}

	// Sweep every hour or every retention period, whichever is shorter.
	interval := 1 * time.Hour
	if retention < interval {
		interval = retention
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepDeletedApps(srv)
		}
	}
}

func sweepDeletedApps(srv *server.Server) {
	retention := srv.Config.Storage.SoftDeleteRetention.Duration
	cutoff := time.Now().Add(-retention).UTC().Format(time.RFC3339)

	apps, err := srv.DB.ListExpiredDeletedApps(cutoff)
	if err != nil {
		slog.Warn("soft-delete sweeper: list failed", "error", err)
		return
	}

	if len(apps) == 0 {
		return
	}

	slog.Info("soft-delete sweeper: purging expired apps", "count", len(apps))
	for _, app := range apps {
		StopAppSync(srv, app.ID)
		PurgeApp(srv, &app)
	}
}

// SpawnLogRetentionCleaner periodically prunes expired log entries.
// Blocks until ctx is cancelled.
func SpawnLogRetentionCleaner(ctx context.Context, srv *server.Server) {
	retention := srv.Config.Proxy.LogRetention.Duration
	interval := retention
	if interval > 60*time.Second || interval <= 0 {
		interval = 60 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n := srv.LogStore.CleanupExpired(retention)
			if n > 0 {
				slog.Debug("log retention: cleaned up entries",
					"count", n)
			}
		}
	}
}
