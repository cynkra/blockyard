package server

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/manifest"
)

// RunRefreshScheduler checks active apps with a refresh_schedule and
// triggers refresh at the configured times. Blocks until ctx is cancelled.
func (srv *Server) RunRefreshScheduler(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			apps, err := srv.DB.ListAppsWithRefreshSchedule()
			if err != nil {
				slog.Warn("refresh scheduler: list apps failed", "error", err)
				continue
			}
			for _, app := range apps {
				if shouldRun(app.RefreshSchedule, app.LastRefreshAt, now) {
					app := app // capture loop variable
					go srv.triggerRefresh(ctx, &app)
				}
			}
		}
	}
}

// shouldRun returns true if the cron expression fires between lastRun and now.
func shouldRun(schedule string, lastRun *string, now time.Time) bool {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(schedule)
	if err != nil {
		slog.Warn("refresh scheduler: invalid cron expression",
			"schedule", schedule, "error", err)
		return false
	}

	var lastTime time.Time
	if lastRun != nil && *lastRun != "" {
		lastTime, _ = time.Parse(time.RFC3339, *lastRun)
	}
	if lastTime.IsZero() {
		// Never run before — fire on next tick.
		lastTime = now.Add(-2 * time.Minute)
	}

	// The schedule should have fired between lastRun and now.
	next := sched.Next(lastTime)
	return !next.After(now)
}

// triggerRefresh runs a refresh for the given app.
func (srv *Server) triggerRefresh(ctx context.Context, app *db.AppRow) {
	if app.ActiveBundle == nil {
		return
	}

	manifestPath := filepath.Join(
		srv.BundlePaths(app.ID, *app.ActiveBundle).Base,
		"manifest.json")
	m, err := manifest.Read(manifestPath)
	if err != nil {
		slog.Warn("refresh scheduler: read manifest",
			"app_id", app.ID, "error", err)
		return
	}
	if m.IsPinned() {
		return // pinned deployments can't be refreshed
	}

	taskID := uuid.New().String()
	sender := srv.Tasks.Create(taskID, app.ID)

	slog.Info("refresh scheduler: triggering refresh",
		"app_id", app.ID, "task_id", taskID)

	srv.RunRefresh(ctx, app, m, sender)

	// Record the refresh time.
	if err := srv.DB.UpdateLastRefresh(app.ID, time.Now()); err != nil {
		slog.Warn("refresh scheduler: update last refresh",
			"app_id", app.ID, "error", err)
	}
}
