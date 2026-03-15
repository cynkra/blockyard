package bundle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/rvcache"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/telemetry"
)

// RestoreParams holds everything the restore goroutine needs.
type RestoreParams struct {
	Backend      backend.Backend
	DB           *db.DB
	Tasks        *task.Store
	Sender       task.Sender
	AppID        string
	BundleID     string
	Paths        Paths
	Image        string
	RvVersion    string
	RvBinaryPath string // if set, skip download and use this path directly
	Retention    int
	BasePath     string // bundle_server_path for retention cleanup
	AuditLog     *audit.Log
	AuditActor   string // sub of the user who triggered the upload
}

// SpawnRestore launches the restore pipeline in a background goroutine.
// Returns immediately.
func SpawnRestore(params RestoreParams) {
	go func() {
		slog.Info("bundle restore started",
			"app_id", params.AppID, "bundle_id", params.BundleID)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("restore task panicked",
					"app_id", params.AppID,
					"bundle_id", params.BundleID,
					"panic", r)
				params.Sender.Write(fmt.Sprintf("FATAL: restore task panicked: %v", r))
				params.Sender.Complete(task.Failed)
				telemetry.BundleRestoresFailed.Inc()
				if params.AuditLog != nil {
					params.AuditLog.Emit(audit.Entry{
						Action: audit.ActionBundleRestoreFail,
						Actor:  params.AuditActor,
						Target: params.AppID,
						Detail: map[string]any{"bundle_id": params.BundleID},
					})
				}
				if err := params.DB.UpdateBundleStatus(params.BundleID, "failed"); err != nil {
					slog.Error("restore: update status to failed after panic",
						"bundle_id", params.BundleID, "error", err)
				}
			}
		}()
		buildStart := time.Now()
		err := runRestore(params)
		if err != nil {
			slog.Warn("bundle restore failed",
				"app_id", params.AppID, "bundle_id", params.BundleID,
				"elapsed", time.Since(buildStart).Round(time.Millisecond),
				"error", err)
			params.Sender.Write(fmt.Sprintf("ERROR: %s", err))
			params.Sender.Complete(task.Failed)
			telemetry.BundleRestoresFailed.Inc()
			if params.AuditLog != nil {
				params.AuditLog.Emit(audit.Entry{
					Action: audit.ActionBundleRestoreFail,
					Actor:  params.AuditActor,
					Target: params.AppID,
					Detail: map[string]any{"bundle_id": params.BundleID},
				})
			}
			if err := params.DB.UpdateBundleStatus(params.BundleID, "failed"); err != nil {
				slog.Error("restore: update status to failed",
					"bundle_id", params.BundleID, "error", err)
			}
			return
		}
		elapsed := time.Since(buildStart)
		slog.Info("bundle restore succeeded",
			"app_id", params.AppID, "bundle_id", params.BundleID,
			"elapsed", elapsed.Round(time.Millisecond))
		telemetry.BuildDuration.Observe(elapsed.Seconds())
		telemetry.BundleRestoresSucceeded.Inc()
		if params.AuditLog != nil {
			params.AuditLog.Emit(audit.Entry{
				Action: audit.ActionBundleRestoreOK,
				Actor:  params.AuditActor,
				Target: params.AppID,
				Detail: map[string]any{"bundle_id": params.BundleID},
			})
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
	slog.Info("bundle state transition",
		"app_id", p.AppID, "bundle_id", p.BundleID, "status", "building")
	p.Sender.Write("Starting dependency restoration...")

	// 2. Ensure rv binary is cached
	rvBinaryPath := p.RvBinaryPath
	if rvBinaryPath == "" {
		cacheDir := filepath.Join(p.BasePath, ".rv-cache")
		var rvErr error
		rvBinaryPath, rvErr = rvcache.EnsureBinary(context.Background(), cacheDir, p.RvVersion)
		if rvErr != nil {
			return fmt.Errorf("ensure rv binary: %w", rvErr)
		}
	}

	// Sanity-check: verify the rv binary is a regular file. In Docker-in-Docker
	// setups, a missing bind-mount source gets auto-created as a directory,
	// producing a confusing "is a directory: permission denied" error.
	if fi, err := os.Stat(rvBinaryPath); err != nil {
		return fmt.Errorf("rv binary not found at %s: %w", rvBinaryPath, err)
	} else if fi.IsDir() {
		return fmt.Errorf("rv binary path %s is a directory, not a file", rvBinaryPath)
	}

	// 3. Set library path in rproject.toml so rv writes to the mounted volume.
	if err := SetLibraryPath(p.Paths, BuildContainerLibPath); err != nil {
		return fmt.Errorf("set library path: %w", err)
	}

	// 4. Build the spec
	labels := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    p.AppID,
		"dev.blockyard/bundle-id": p.BundleID,
	}

	spec := backend.BuildSpec{
		AppID:        p.AppID,
		BundleID:     p.BundleID,
		Image:        p.Image,
		RvBinaryPath: rvBinaryPath,
		BundlePath:   p.Paths.Unpacked,
		LibraryPath:  p.Paths.Library,
		Labels:       labels,
		LogWriter:    p.Sender.Write,
	}

	// 3. Run the build
	result, err := p.Backend.Build(context.Background(), spec)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("build failed with exit code %d", result.ExitCode)
	}

	// 4. Mark bundle as ready and activate (atomic).
	p.Sender.Write("Build succeeded. Activating bundle...")
	slog.Info("bundle state transition",
		"app_id", p.AppID, "bundle_id", p.BundleID, "status", "activating")

	if err := p.DB.ActivateBundle(p.AppID, p.BundleID); err != nil {
		return fmt.Errorf("activate bundle: %w", err)
	}

	slog.Info("bundle state transition",
		"app_id", p.AppID, "bundle_id", p.BundleID, "status", "active")
	p.Sender.Write("Bundle activated.")
	return nil
}
