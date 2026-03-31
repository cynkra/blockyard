package update

import (
	"context"
	"log/slog"
	"time"
)

// UpdateStore is the interface the checker needs to record its result.
// Satisfied by server.Server via its UpdateAvailable atomic field.
type UpdateStore interface {
	SetUpdateAvailable(version string)
	GetVersion() string
}

// SpawnChecker periodically checks GitHub for a newer release and stores
// the result. It performs an initial check after 1 minute, then every 24
// hours. Blocks until ctx is cancelled.
func SpawnChecker(ctx context.Context, version string, store UpdateStore) {
	// Initial delay — let the server finish starting up.
	select {
	case <-ctx.Done():
		return
	case <-time.After(1 * time.Minute):
	}

	check(version, store)

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check(version, store)
		}
	}
}

func check(version string, store UpdateStore) {
	channel := InferChannel(version)
	result, err := CheckLatest(channel, version)
	if err != nil {
		slog.Warn("update check failed", "error", err)
		return
	}
	if result.UpdateAvailable {
		store.SetUpdateAvailable(result.LatestVersion)
		slog.Warn("a newer version is available",
			"current", version,
			"latest", result.LatestVersion,
			"channel", channel,
		)
	}
}
