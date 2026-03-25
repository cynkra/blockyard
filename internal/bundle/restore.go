package bundle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/pakcache"
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
	PakVersion   string // "stable" (default), or pinned version
	PakCachePath string // base directory for pak cache
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
	p.Sender.Write("restoring dependencies...")

	// 2. Ensure pak is cached.
	pakCachePath := p.PakCachePath
	if pakCachePath == "" {
		pakCachePath = filepath.Join(p.BasePath, ".pak-cache")
	}
	pakPath, err := pakcache.EnsureInstalled(
		context.Background(), p.Backend,
		p.Image, p.PakVersion, pakCachePath)
	if err != nil {
		return fmt.Errorf("ensure pak: %w", err)
	}

	// 3. Resolve manifest from bundle contents.
	m, err := resolveManifest(p.Paths.Unpacked)
	if err != nil {
		return fmt.Errorf("resolve manifest: %w", err)
	}

	// 4. Bare scripts: pre-process to generate DESCRIPTION, then retry.
	if m == nil {
		p.Sender.Write("scanning scripts for dependencies...")
		if err := preProcess(context.Background(), p.Backend, pakPath, p); err != nil {
			return fmt.Errorf("preprocess: %w", err)
		}
		m, err = resolveManifest(p.Paths.Unpacked)
		if err != nil {
			return fmt.Errorf("resolve manifest after preprocess: %w", err)
		}
		if m == nil {
			return errors.New("failed to produce manifest after preprocessing")
		}
	}

	// 5. Write manifest.json to unpacked dir (if generated server-side).
	manifestPath := filepath.Join(p.Paths.Unpacked, "manifest.json")
	if !fileExists(manifestPath) {
		if err := m.Write(manifestPath); err != nil {
			return fmt.Errorf("write manifest: %w", err)
		}
	}

	mode := m.BuildMode()
	p.Sender.Write(fmt.Sprintf("build mode: %s", mode))

	// 6. Ensure download cache dir exists.
	dlCachePath := filepath.Join(p.BasePath, ".pak-dl-cache")
	if err := os.MkdirAll(dlCachePath, 0o755); err != nil {
		return fmt.Errorf("create download cache dir: %w", err)
	}

	// 7. Run build container.
	spec := backend.BuildSpec{
		AppID:    p.AppID,
		BundleID: p.BundleID,
		Image:    p.Image,
		Cmd:      buildCommand(),
		Mounts:   buildMounts(pakPath, p.Paths.Unpacked, p.Paths.Library, dlCachePath),
		Labels: map[string]string{
			"dev.blockyard/managed":   "true",
			"dev.blockyard/role":      "build",
			"dev.blockyard/app-id":    p.AppID,
			"dev.blockyard/bundle-id": p.BundleID,
		},
		LogWriter: func(line string) { p.Sender.Write(line) },
	}

	result, err := p.Backend.Build(context.Background(), spec)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if !result.Success {
		slog.Error("build container failed",
			"app_id", p.AppID, "bundle_id", p.BundleID,
			"exit_code", result.ExitCode, "logs", result.Logs)
		return fmt.Errorf("dependency restore failed (exit %d)", result.ExitCode)
	}

	// 8. Persist pak lockfile alongside bundle.
	lockfileSrc := filepath.Join(p.Paths.Library, "pak.lock")
	lockfileDst := filepath.Join(p.Paths.Base, "pak.lock")
	if err := copyFile(lockfileSrc, lockfileDst); err != nil {
		slog.Warn("failed to persist pak lockfile",
			"error", err, "bundle_id", p.BundleID)
		// Non-fatal — the build succeeded, lockfile is a downstream
		// optimization for store assembly (phase 2-6) and refresh (2-7).
	}

	// 9. Persist manifest alongside bundle.
	manifestDst := filepath.Join(p.Paths.Base, "manifest.json")
	if err := m.Write(manifestDst); err != nil {
		slog.Warn("failed to persist manifest",
			"error", err, "bundle_id", p.BundleID)
	}

	// 10. Activate bundle.
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

// buildCommand returns the R command that runs inside the build container.
// The R script handles both pinned and unpinned modes — it reads the
// manifest to determine which.
func buildCommand() []string {
	rScript := `
		.libPaths(c("/pak", .libPaths()))
		library(pak)
		Sys.setenv(PKG_CACHE_DIR = "/pak-cache")

		# simplifyVector = FALSE: prevents jsonlite from collapsing
		# the packages list into a data frame when all records have
		# identical field sets. vapply() over a data frame iterates
		# columns, not rows — silently producing wrong refs.
		manifest <- jsonlite::fromJSON("/app/manifest.json",
		                               simplifyVector = FALSE)

		# Configure repos.
		if (length(manifest$repositories) > 0) {
		  repo_urls <- setNames(
		    vapply(manifest$repositories, function(r) {
		      url <- r$URL
		      if (grepl("p3m\\.dev|packagemanager\\.posit\\.co", url) &&
		          !grepl("__linux__", url)) {
		        os_rel <- readLines("/etc/os-release")
		        cn <- sub("^VERSION_CODENAME=", "",
		                  grep("^VERSION_CODENAME=", os_rel, value = TRUE))
		        url <- sub("(/cran/|/bioc/)",
		                   paste0("\\1__linux__/", cn, "/"), url)
		      }
		      url
		    }, ""),
		    vapply(manifest$repositories, function(r) r$Name, "")
		  )
		  options(repos = repo_urls)
		}

		# Derive refs.
		record_to_ref <- function(rec) {
		  switch(rec$Source,
		    Repository =, Bioconductor = {
		      prefix <- if (rec$Source == "Bioconductor") "bioc::" else ""
		      paste0(prefix, rec$Package, "@", rec$Version)
		    },
		    GitHub =    paste0(rec$RemoteUsername, "/", rec$RemoteRepo,
		                      "@", rec$RemoteSha),
		    GitLab =    paste0("gitlab::", rec$RemoteUsername, "/",
		                      rec$RemoteRepo, "@", rec$RemoteSha),
		    Bitbucket = paste0("bitbucket::", rec$RemoteUsername, "/",
		                      rec$RemoteRepo, "@", rec$RemoteSha),
		    git    =    paste0("git::", rec$RemoteUrl),
		    stop("Unsupported Source: ", rec$Source)
		  )
		}

		if (!is.null(manifest$packages)) {
		  refs <- vapply(manifest$packages, record_to_ref, "")
		} else {
		  refs <- "deps::/app"
		}

		pak::lockfile_create(refs,
		  lockfile = "/build-lib/pak.lock", lib = "/build-lib")
		pak::lockfile_install("/build-lib/pak.lock", lib = "/build-lib")
	`
	return []string{"R", "--vanilla", "-e", rScript}
}

// buildMounts returns the mount entries for the pak build container.
func buildMounts(
	pakCachePath, bundlePath, libraryPath, dlCachePath string,
) []backend.MountEntry {
	return []backend.MountEntry{
		{Source: bundlePath, Target: "/app", ReadOnly: true},
		{Source: libraryPath, Target: "/build-lib", ReadOnly: false},
		{Source: pakCachePath, Target: "/pak", ReadOnly: true},
		{Source: dlCachePath, Target: "/pak-cache", ReadOnly: false},
	}
}
