package update

import (
	"context"
	"log/slog"
	"time"
)

// UpdateStore is the interface a long-lived caller (the server)
// implements to receive check results. Satisfied by server.Server.
type UpdateStore interface {
	SetUpdateStatus(*Result)
	GetVersion() string
}

// SpawnChecker periodically checks GitHub for a newer release on the
// given channel and stores the result on store. Performs an initial
// check after one minute (so startup isn't slowed by network), then
// once every 24 hours. Blocks until ctx is cancelled.
func SpawnChecker(ctx context.Context, version, channel string, store UpdateStore) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(1 * time.Minute):
	}

	_, _ = PerformCheck(store, channel)

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = PerformCheck(store, channel)
		}
	}
}

// PerformCheck runs a single update check synchronously against the
// given channel and writes the outcome to store. Always writes —
// even when the result is "up to date" or "dev build" — so the UI
// can display the most recent check time. Returns the result so a
// UI handler can render it directly.
func PerformCheck(store UpdateStore, channel string) (*Result, error) {
	result, err := CheckLatest(store.GetVersion(), channel)
	if err != nil {
		return nil, err
	}
	store.SetUpdateStatus(result)
	if result.State == StateUpdateAvailable {
		slog.Warn("a newer version is available",
			"current", result.CurrentVersion,
			"latest", result.LatestVersion,
			"channel", result.Channel,
			"detail", result.Detail,
		)
	}
	return result, nil
}
