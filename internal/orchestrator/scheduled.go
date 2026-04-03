package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/cynkra/blockyard/internal/task"
)

// RunScheduled checks for updates on the configured cron schedule.
// When an update is available, it triggers the full update + watchdog
// flow. Blocks until ctx is cancelled.
//
// Uses o.exitFn to signal the main goroutine — RunScheduled is a
// bgWg goroutine and cannot call Finish directly (deadlock).
func (o *Orchestrator) RunScheduled(
	ctx context.Context,
	schedule string,
	channel string,
) {
	if channel == "" {
		channel = "stable"
	}

	parser := cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(schedule)
	if err != nil {
		slog.Error("update scheduler: invalid cron expression",
			"schedule", schedule, "error", err)
		return
	}

	slog.Info("update scheduler started",
		"schedule", schedule, "channel", channel)

	for {
		next := sched.Next(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}

		if o.runScheduledOnce(ctx, channel) {
			return // update succeeded, main will exit
		}
	}
}

// runScheduledOnce checks for an update and runs the full update+watchdog
// flow if one is available. Returns true when the caller should exit
// (successful update), false to continue the schedule loop.
func (o *Orchestrator) runScheduledOnce(ctx context.Context, channel string) bool {
	slog.Info("update scheduler: checking for updates")
	result, err := o.update.CheckLatest(channel, o.version)
	if err != nil {
		slog.Warn("update scheduler: check failed", "error", err)
		return false
	}
	if !result.UpdateAvailable {
		slog.Info("update scheduler: already up to date")
		return false
	}

	if !o.CASState("idle", "updating") {
		slog.Info("update scheduler: skipping, another operation in progress",
			"state", o.State())
		return false
	}

	slog.Info("update scheduler: starting update",
		"current", result.CurrentVersion,
		"latest", result.LatestVersion)

	sender := o.tasks.Create(uuid.New().String(), "scheduled-update")
	ur, err := o.Update(ctx, channel, sender)
	if err != nil {
		slog.Error("update scheduler: update failed", "error", err)
		sender.Complete(task.Failed)
		o.state.Store("idle")
		return false
	}
	if ur == nil {
		sender.Complete(task.Completed)
		o.state.Store("idle")
		return false
	}

	watchPeriod := 5 * time.Minute
	if o.cfg.Update != nil && o.cfg.Update.WatchPeriod.Duration > 0 {
		watchPeriod = o.cfg.Update.WatchPeriod.Duration
	}
	if err := o.Watchdog(ctx, ur.ContainerID, ur.Addr, watchPeriod, sender); err != nil {
		slog.Error("update scheduler: watchdog rollback", "error", err)
		sender.Complete(task.Failed)
		return false
	}

	sender.Complete(task.Completed)
	o.exitFn()
	return true
}
