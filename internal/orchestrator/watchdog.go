package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/cynkra/blockyard/internal/task"
)

// watchdogFailureThreshold is the number of consecutive /readyz
// failures required before the watchdog triggers a rollback. A single
// transient failure (GC pause, brief DB pool exhaustion, slow IdP
// response) should not undo a successful deployment.
var watchdogFailureThreshold = 3

// Watchdog monitors the new server after a successful update. It
// reads the target instance from o.activeInstance (set by Update) so
// the admin handler doesn't thread an opaque handle through the API
// layer.
//
// If the new server becomes unhealthy (3 consecutive failures) within
// the watch period, the watchdog kills the new instance, un-drains,
// and resumes serving.
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

	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				sender.Write("Watch period elapsed. New server healthy. Exiting.")
				return nil // caller exits the process
			}
			if err := o.checkReady(ctx, addr); err != nil {
				consecutiveFailures++
				sender.Write(fmt.Sprintf(
					"New server unhealthy (%d/%d): %v",
					consecutiveFailures, watchdogFailureThreshold, err))
				if consecutiveFailures >= watchdogFailureThreshold {
					sender.Write("Failure threshold reached. Rolling back.")
					inst.Kill(ctx)
					o.undrainFn()
					sender.Write("Rolled back. Old server resumed.")
					return fmt.Errorf("watchdog: new server failed after %d consecutive checks: %w",
						consecutiveFailures, err)
				}
			} else {
				consecutiveFailures = 0
			}
		}
	}
}
