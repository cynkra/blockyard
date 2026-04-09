package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/cynkra/blockyard/internal/task"
)

// Watchdog monitors the new server after a successful update. It
// reads the target instance from o.activeInstance (set by Update) so
// the admin handler doesn't thread an opaque handle through the API
// layer.
//
// If the new server becomes unhealthy within the watch period, the
// watchdog kills the new instance, un-drains, and resumes serving.
//
// If the new server stays healthy for the full period, the old
// server exits (returns nil, caller signals main goroutine).
func (o *Orchestrator) Watchdog(
	ctx context.Context,
	watchPeriod time.Duration,
	sender task.Sender,
) error {
	o.state.Store("watching")
	defer func() {
		o.state.Store("idle")
		o.activeInstance = nil
	}()

	inst := o.activeInstance
	if inst == nil {
		return fmt.Errorf("watchdog: no active instance (internal error)")
	}
	addr := inst.Addr()

	deadline := time.Now().Add(watchPeriod)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := o.checkReady(ctx, addr); err != nil {
				sender.Write(fmt.Sprintf(
					"New server unhealthy: %v. Rolling back.", err))
				inst.Kill(ctx)
				o.undrainFn()
				sender.Write("Rolled back. Old server resumed.")
				return fmt.Errorf("watchdog: new server failed: %w", err)
			}
			if time.Now().After(deadline) {
				sender.Write("Watch period elapsed. New server healthy. Exiting.")
				return nil // caller exits the process
			}
		}
	}
}
