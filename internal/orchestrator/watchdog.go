package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/cynkra/blockyard/internal/task"
)

// Watchdog monitors the new server after a successful update.
// If the new server becomes unhealthy within the watch period,
// it kills the new container, un-drains, and resumes serving.
//
// If the new server stays healthy for the full period, the old
// server exits (returns nil, caller signals main goroutine).
func (o *Orchestrator) Watchdog(
	ctx context.Context,
	newID string,
	newAddr string,
	watchPeriod time.Duration,
	sender task.Sender,
) error {
	o.state.Store("watching")
	defer o.state.Store("idle")

	deadline := time.Now().Add(watchPeriod)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				sender.Write("Watch period elapsed. New server healthy. Exiting.")
				return nil // caller exits the process
			}
			if err := o.checkReady(ctx, newAddr); err != nil {
				sender.Write(fmt.Sprintf(
					"New server unhealthy: %v. Rolling back.", err))
				o.killAndRemove(ctx, newID)
				o.undrainFn()
				sender.Write("Rolled back. Old server resumed.")
				return fmt.Errorf("watchdog: new server failed: %w", err)
			}
		}
	}
}
