package bundle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/buildercache"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/pakcache"
	"github.com/cynkra/blockyard/internal/pkgstore"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/telemetry"
)

// RestoreParams holds everything the restore goroutine needs.
type RestoreParams struct {
	Ctx              context.Context // optional; defaults to context.Background()
	Backend          backend.Backend
	DB               *db.DB
	Tasks            *task.Store
	Sender           task.Sender
	AppID            string
	BundleID         string
	Paths            Paths
	Image            string
	PakVersion       string // "stable" (default), or pinned version
	PakCachePath     string // base directory for pak cache
	BuilderVersion   string // by-builder binary version
	BuilderCachePath string // by-builder cache directory
	Retention        int
	BasePath         string // bundle_server_path for retention cleanup
	Store            *pkgstore.Store
	AuditLog         *audit.Log
	AuditActor       string // sub of the user who triggered the upload
	Metrics          *telemetry.Metrics // records restore duration + outcome
	WG               *sync.WaitGroup    // if non-nil, Add(1) before goroutine, Done() on exit
}

// SpawnRestore launches the restore pipeline in a background goroutine.
// Returns a channel that is closed when the goroutine finishes.
func SpawnRestore(params RestoreParams) <-chan struct{} {
	done := make(chan struct{})
	if params.WG != nil {
		params.WG.Add(1)
	}
	go func() {
		defer close(done)
		if params.WG != nil {
			defer params.WG.Done()
		}
		slog.Info("bundle restore started",
			"app_id", params.AppID, "bundle_id", params.BundleID)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("restore task panicked",
					"app_id", params.AppID,
					"bundle_id", params.BundleID,
					"panic", r)
				params.Sender.Write("FATAL: an unexpected error occurred during build.")
				params.Sender.Complete(task.Failed)
				if params.Metrics != nil {
					params.Metrics.BundleRestoresFailed.Inc()
				}
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
			if params.Metrics != nil {
				params.Metrics.BundleRestoresFailed.Inc()
			}
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
		if params.Metrics != nil {
			params.Metrics.BuildDuration.Observe(elapsed.Seconds())
			params.Metrics.BundleRestoresSucceeded.Inc()
		}
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
	return done
}

func runRestore(p RestoreParams) error {
	ctx := p.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	// 1. Update status to "building"
	if err := p.DB.UpdateBundleStatus(p.BundleID, "building"); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	slog.Info("bundle state transition",
		"app_id", p.AppID, "bundle_id", p.BundleID, "status", "building")
	p.Sender.Write("restoring dependencies...")

	// 2. Ensure pak and by-builder are cached.
	pakCachePath := p.PakCachePath
	if pakCachePath == "" {
		pakCachePath = filepath.Join(p.BasePath, ".pak-cache")
	}
	pakPath, err := pakcache.EnsureInstalled(
		ctx, p.Backend,
		p.Image, p.PakVersion, pakCachePath)
	if err != nil {
		return fmt.Errorf("set up build tools: %w", err)
	}
	// Only cache by-builder when the package store is available.
	var builderPath string
	if p.Store != nil {
		builderCachePath := p.BuilderCachePath
		if builderCachePath == "" {
			builderCachePath = filepath.Join(p.BasePath, ".by-builder-cache")
		}
		builderPath, err = buildercache.EnsureCached(
			builderCachePath, p.BuilderVersion)
		if err != nil {
			return fmt.Errorf("set up build tools: %w", err)
		}
	}

	// 3. Resolve manifest from bundle contents.
	m, err := resolveManifest(p.Paths.Unpacked)
	if err != nil {
		return fmt.Errorf("resolve manifest: %w", err)
	}

	// 4. Handle manifest / bare scripts.
	if p.Store != nil {
		// Store-aware: bare scripts handled directly by R build script
		// (scan_deps). Manifest written if generated server-side.
		if m != nil {
			manifestPath := filepath.Join(p.Paths.Unpacked, "manifest.json")
			if !fileExists(manifestPath) {
				if err := m.Write(manifestPath); err != nil {
					return fmt.Errorf("write manifest: %w", err)
				}
			}
			p.Sender.Write(fmt.Sprintf("build mode: %s", m.BuildMode()))
		} else {
			p.Sender.Write("build mode: auto-detected from scripts")
		}
	} else {
		// Legacy: bare scripts need preProcess container, manifest
		// needs .pak-refs/.pak-repos text files.
		if m == nil {
			p.Sender.Write("scanning scripts for dependencies...")
			if err := preProcess(ctx, p.Backend, pakPath, p); err != nil {
				return fmt.Errorf("preprocess: %w", err)
			}
			m, err = resolveManifest(p.Paths.Unpacked)
			if err != nil {
				return fmt.Errorf("resolve manifest after preprocess: %w", err)
			}
			if m == nil {
				return fmt.Errorf("failed to produce manifest after preprocessing")
			}
		}
		manifestPath := filepath.Join(p.Paths.Unpacked, "manifest.json")
		if !fileExists(manifestPath) {
			if err := m.Write(manifestPath); err != nil {
				return fmt.Errorf("write manifest: %w", err)
			}
		}
		p.Sender.Write(fmt.Sprintf("build mode: %s", m.BuildMode()))

		// Write pak refs and repos for the legacy R build script.
		refsData := strings.Join(m.PakRefs(), "\n") + "\n"
		if err := os.WriteFile(filepath.Join(p.Paths.Unpacked, ".pak-refs"), []byte(refsData), 0o644); err != nil { //nolint:gosec // G306: pak-refs metadata, not secrets
			return fmt.Errorf("write pak refs: %w", err)
		}
		if lines := m.RepoLines(); len(lines) > 0 {
			repoData := strings.Join(lines, "\n") + "\n"
			if err := os.WriteFile(filepath.Join(p.Paths.Unpacked, ".pak-repos"), []byte(repoData), 0o644); err != nil { //nolint:gosec // G306: pak-repos metadata, not secrets
				return fmt.Errorf("write pak repos: %w", err)
			}
		}
	}

	// 5. Ensure download cache dir exists.
	dlCachePath := filepath.Join(p.BasePath, ".pak-dl-cache")
	if err := os.MkdirAll(dlCachePath, 0o755); err != nil { //nolint:gosec // G301: download cache dir, not secrets
		return fmt.Errorf("create download cache dir: %w", err)
	}

	// 6. Ensure store dir exists and prepare build.
	if p.Store != nil {
		if err := os.MkdirAll(p.Store.Root(), 0o755); err != nil { //nolint:gosec // G301: package store dir, not secrets
			return fmt.Errorf("create store dir: %w", err)
		}
	}

	// 7. Generate build UUID for the build library path.
	buildUUID := uuid.New().String()

	// R version from the manifest for version dispatch in the process
	// backend. Empty for bare-script bundles (no renv.lock).
	var rVersion string
	if m != nil {
		rVersion = m.RVersion
	}

	// 8. Run build container.
	var spec backend.BuildSpec
	if p.Store != nil {
		// Store-aware build: four-phase pipeline with by-builder.
		spec = backend.BuildSpec{
			AppID:    p.AppID,
			BundleID: p.BundleID,
			Image:    p.Image,
			Cmd:      BuildCommand(),
			Mounts:   BuildMounts(pakPath, p.Paths.Unpacked, p.Store.Root(), dlCachePath, builderPath),
			Env:      []string{"BUILD_UUID=" + buildUUID},
			Labels: map[string]string{
				"dev.blockyard/managed":   "true",
				"dev.blockyard/role":      "build",
				"dev.blockyard/app-id":    p.AppID,
				"dev.blockyard/bundle-id": p.BundleID,
			},
			LogWriter: func(line string) { p.Sender.Write(line) },
			RVersion:  rVersion,
		}
	} else {
		// Legacy build (no store): phase 2-5 flow.
		spec = backend.BuildSpec{
			AppID:    p.AppID,
			BundleID: p.BundleID,
			Image:    p.Image,
			Cmd:      legacyBuildCommand(),
			Mounts:   legacyBuildMounts(pakPath, p.Paths.Unpacked, p.Paths.Library, dlCachePath),
			Labels: map[string]string{
				"dev.blockyard/managed":   "true",
				"dev.blockyard/role":      "build",
				"dev.blockyard/app-id":    p.AppID,
				"dev.blockyard/bundle-id": p.BundleID,
			},
			LogWriter: func(line string) { p.Sender.Write(line) },
			RVersion:  rVersion,
		}
	}

	result, err := p.Backend.Build(ctx, spec)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// Persist build log regardless of outcome.
	_ = p.DB.InsertBundleLog(p.BundleID, result.Logs)

	if !result.Success {
		slog.Error("build container failed",
			"app_id", p.AppID, "bundle_id", p.BundleID,
			"exit_code", result.ExitCode, "logs", result.Logs)
		return fmt.Errorf("dependency restore failed (exit %d)", result.ExitCode)
	}

	// 9. Post-build artifact extraction.
	if p.Store != nil {
		buildDir := filepath.Join(p.Store.Root(), ".builds", buildUUID)
		defer func() { _ = os.RemoveAll(buildDir) }()

		// store-manifest.json is required — it drives assembly and rollback.
		manifestSrc := filepath.Join(buildDir, "store-manifest.json")
		manifestDst := filepath.Join(p.Paths.Base, "store-manifest.json")
		if err := copyFile(manifestSrc, manifestDst); err != nil {
			return fmt.Errorf("save dependency manifest: %w", err)
		}

		// Persist immutable baseline from the original deploy.
		buildManifest := filepath.Join(p.Paths.Base, "store-manifest.json.build")
		if err := copyFile(manifestSrc, buildManifest); err != nil {
			return fmt.Errorf("save dependency manifest: %w", err)
		}

		// pak.lock — debug/audit artifact; extraction failure is non-fatal.
		lockfileSrc := filepath.Join(buildDir, "pak.lock")
		lockfileDst := filepath.Join(p.Paths.Base, "pak.lock")
		if err := copyFile(lockfileSrc, lockfileDst); err != nil {
			slog.Warn("failed to persist pak lockfile (debug artifact)",
				"error", err, "bundle_id", p.BundleID)
		}

		// Set store platform from lockfile. Updated on every build
		// (not just the first) so that an R version upgrade is
		// picked up without a server restart.
		if lf, err := pkgstore.ReadLockfile(lockfileDst); err == nil {
			p.Store.SetPlatform(pkgstore.PlatformFromLockfile(lf))
		}
	} else {
		// Legacy: persist pak lockfile alongside bundle.
		lockfileSrc := filepath.Join(p.Paths.Library, "pak.lock")
		lockfileDst := filepath.Join(p.Paths.Base, "pak.lock")
		if err := copyFile(lockfileSrc, lockfileDst); err != nil {
			slog.Warn("failed to persist pak lockfile",
				"error", err, "bundle_id", p.BundleID)
		}
	}

	// 10. Persist manifest alongside bundle.
	if m != nil {
		canonicalManifest := filepath.Join(p.Paths.Base, "manifest.json")
		if err := m.Write(canonicalManifest); err != nil {
			slog.Warn("failed to persist manifest",
				"error", err, "bundle_id", p.BundleID)
		}
	}

	// 11. Activate bundle.
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

// BuildCommand returns the R command that runs inside the build container.
// The four-phase store-aware build script:
//   - Phase 1: lockfile_create (pak resolves + solves)
//   - Phase 2: by-builder store populate (pre-populate from store)
//   - Phase 3: lockfile_install (install store misses)
//   - Phase 4: by-builder store ingest (ingest into store)
//
// Exported for use by the refresh pipeline (server/refresh.go).
func BuildCommand() []string {
	rScript := `
Sys.setenv(
  R_USER_CACHE_DIR = "/pak-cache",
  PKG_CACHE_DIR = "/pak-cache",
  PKG_SYSREQS = "false"
)
.libPaths(c("/pak", .libPaths()))
library(pak)
# Make pak's bundled dependencies (jsonlite, pkgdepends) available.
pak_lib <- system.file("library", package = "pak")
if (nzchar(pak_lib) && dir.exists(pak_lib)) {
  .libPaths(c(pak_lib, .libPaths()))
}

# -- Read manifest (if present) and configure -------------------------
manifest <- NULL
if (file.exists("/app/manifest.json")) {
  manifest <- jsonlite::fromJSON("/app/manifest.json",
                                 simplifyVector = FALSE)
}

if (!is.null(manifest) && length(manifest$repositories) > 0) {
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

# -- Derive refs -------------------------------------------------------
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

if (!is.null(manifest) && !is.null(manifest$packages)) {
  # Pinned: convert each package record to a pkgdepends ref.
  refs <- vapply(manifest$packages, record_to_ref, "")
} else if (!is.null(manifest) && !is.null(manifest$description)) {
  # Unpinned with DESCRIPTION: pak reads Imports/Depends/Remotes.
  refs <- "deps::/app"
} else if (file.exists("/app/DESCRIPTION")) {
  # DESCRIPTION without manifest (legacy or direct upload).
  refs <- "deps::/app"
} else {
  # Bare scripts: scan for library()/require()/:: calls directly.
  message("No manifest or DESCRIPTION found -- scanning scripts")
  deps <- pkgdepends::scan_deps(path = "/app", root = "/app")
  refs <- unique(deps$package[deps$type == "prod"])
  if (length(refs) == 0) stop("No package dependencies found in scripts")
}

# Build library lives on the store volume so ingestion is an atomic rename.
build_uuid <- Sys.getenv("BUILD_UUID")
build_lib <- file.path("/store", ".builds", build_uuid)
dir.create(build_lib, recursive = TRUE)

# -- Phase 1: Resolve + solve (no download, no install) ----------------
pak::lockfile_create(refs,
  lockfile = file.path(build_lib, "pak.lock"), lib = build_lib)

# -- Phase 2: Check store, pre-populate build library ------------------
rc <- system2("/tools/by-builder", c(
  "store", "populate",
  "--lockfile", file.path(build_lib, "pak.lock"),
  "--lib", build_lib,
  "--store", "/store"
))
if (rc != 0L) {
  message("WARNING: store populate failed (exit ", rc,
          "); falling back to full install")
}

# -- Phase 3: Install store misses ------------------------------------
pak::lockfile_install(file.path(build_lib, "pak.lock"), lib = build_lib)

# -- Phase 4: Ingest newly installed packages into store ---------------
rc <- system2("/tools/by-builder", c(
  "store", "ingest",
  "--lockfile", file.path(build_lib, "pak.lock"),
  "--lib", build_lib,
  "--store", "/store"
))
if (rc != 0L) {
  stop("store ingest failed (exit ", rc,
       "); store-manifest.json was not written")
}
`
	return []string{"Rscript", "--vanilla", "-e", rScript}
}

// BuildMounts returns the mount entries for the store-aware build container.
// Exported for use by the refresh pipeline (server/refresh.go).
func BuildMounts(
	pakCachePath, bundlePath, storePath, dlCachePath, builderPath string,
) []backend.MountEntry {
	return []backend.MountEntry{
		{Source: bundlePath, Target: "/app", ReadOnly: true},
		{Source: pakCachePath, Target: "/pak", ReadOnly: true},
		{Source: dlCachePath, Target: "/pak-cache", ReadOnly: false},
		{Source: storePath, Target: "/store", ReadOnly: false},
		{Source: builderPath, Target: "/tools/by-builder", ReadOnly: true},
	}
}

// legacyBuildCommand returns the R command for the phase 2-5 build flow
// (no package store). Used when Store is nil (tests, pre-store deployments).
func legacyBuildCommand() []string {
	rScript := `
Sys.setenv(
  R_USER_CACHE_DIR = "/pak-cache",
  PKG_CACHE_DIR = "/pak-cache",
  PKG_SYSREQS = "false"
)
.libPaths(c("/pak", .libPaths()))
library(pak)

# Configure repos from pre-computed text file (Name=URL per line).
repos_file <- "/app/.pak-repos"
if (file.exists(repos_file)) {
  lines <- readLines(repos_file, warn = FALSE)
  lines <- lines[nzchar(lines)]
  if (length(lines) > 0) {
    nms  <- sub("=.*", "", lines)
    urls <- sub("^[^=]+=", "", lines)
    for (i in seq_along(urls)) {
      if (grepl("p3m\\.dev|packagemanager\\.posit\\.co", urls[i]) &&
          !grepl("__linux__", urls[i])) {
        os_rel <- readLines("/etc/os-release")
        cn <- sub("^VERSION_CODENAME=", "",
                  grep("^VERSION_CODENAME=", os_rel, value = TRUE))
        urls[i] <- sub("(/cran/|/bioc/)",
                       paste0("\\1__linux__/", cn, "/"), urls[i])
      }
    }
    options(repos = setNames(urls, nms))
  }
}

# Read refs from pre-computed text file (one ref per line).
refs <- readLines("/app/.pak-refs", warn = FALSE)
refs <- refs[nzchar(refs)]

pak::lockfile_create(refs,
  lockfile = "/build-lib/pak.lock", lib = "/build-lib")
pak::lockfile_install("/build-lib/pak.lock", lib = "/build-lib")
`
	return []string{"Rscript", "--vanilla", "-e", rScript}
}

// legacyBuildMounts returns mount entries for the phase 2-5 build flow.
func legacyBuildMounts(
	pakCachePath, bundlePath, libraryPath, dlCachePath string,
) []backend.MountEntry {
	return []backend.MountEntry{
		{Source: bundlePath, Target: "/app", ReadOnly: true},
		{Source: libraryPath, Target: "/build-lib", ReadOnly: false},
		{Source: pakCachePath, Target: "/pak", ReadOnly: true},
		{Source: dlCachePath, Target: "/pak-cache", ReadOnly: false},
	}
}
