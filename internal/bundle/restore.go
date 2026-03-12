package bundle

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/task"
)

// RestoreParams holds everything the restore goroutine needs.
type RestoreParams struct {
	Backend   backend.Backend
	DB        *db.DB
	Tasks     *task.Store
	Sender    task.Sender
	AppID     string
	BundleID  string
	Paths     Paths
	Image     string
	RvVersion string
	Retention int
	BasePath  string // bundle_server_path for retention cleanup
}

// SpawnRestore launches the restore pipeline in a background goroutine.
// Returns immediately.
func SpawnRestore(params RestoreParams) {
	go func() {
		err := runRestore(params)
		if err != nil {
			params.Sender.Write(fmt.Sprintf("ERROR: %s", err))
			params.Sender.Complete(task.Failed)
			if err := params.DB.UpdateBundleStatus(params.BundleID, "failed"); err != nil {
				slog.Error("restore: update status to failed",
					"bundle_id", params.BundleID, "error", err)
			}
			return
		}
		params.Sender.Complete(task.Completed)
		// Enforce retention after successful deploy
		EnforceRetention(
			params.DB, params.BasePath, params.AppID,
			params.BundleID, params.Retention,
		)
	}()
}

func runRestore(p RestoreParams) error {
	// 1. Update status to "building"
	if err := p.DB.UpdateBundleStatus(p.BundleID, "building"); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	p.Sender.Write("Starting dependency restoration...")

	// 2. Build the spec
	labels := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    p.AppID,
		"dev.blockyard/bundle-id": p.BundleID,
	}

	spec := backend.BuildSpec{
		AppID:       p.AppID,
		BundleID:    p.BundleID,
		Image:       p.Image,
		RvVersion:   p.RvVersion,
		BundlePath:  p.Paths.Unpacked,
		LibraryPath: p.Paths.Library,
		Labels:      labels,
	}

	// 3. Run the build
	result, err := p.Backend.Build(context.Background(), spec)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("build failed with exit code %d", result.ExitCode)
	}

	// 4. Mark bundle as ready and activate
	p.Sender.Write("Build succeeded. Activating bundle...")

	if err := p.DB.UpdateBundleStatus(p.BundleID, "ready"); err != nil {
		return fmt.Errorf("update status to ready: %w", err)
	}
	if err := p.DB.SetActiveBundle(p.AppID, p.BundleID); err != nil {
		return fmt.Errorf("set active bundle: %w", err)
	}

	p.Sender.Write("Bundle activated.")
	return nil
}
