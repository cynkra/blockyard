package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/buildercache"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/pakcache"
	"github.com/cynkra/blockyard/internal/pkgstore"
)

// runtimeInstallParams holds the inputs for a runtime install container.
type runtimeInstallParams struct {
	AppID        string
	WorkerID     string
	Ref          string
	PakPath      string
	BuilderPath  string // path to cached by-builder binary
	StagingDir   string
	WorkerLibDir string
	StoreRoot    string
	Platform     string
	Image        string
	Repositories []manifest.Repository
}

// InstallPackage is the core orchestration for runtime package installation.
// Uses the same four-phase store-aware flow as the build pipeline, differing
// in resolution context: the worker's existing /lib is a reference library
// so pak sees what's installed.
func (srv *Server) InstallPackage(
	ctx context.Context,
	appID, workerID string,
	req PackageRequest,
) (PackageResponse, error) {

	// Serialize runtime installs per-worker.
	mu := srv.workerInstallMu(workerID)
	mu.Lock()
	defer mu.Unlock()

	// 1. Look up the worker and its library path.
	worker, ok := srv.Workers.Get(workerID)
	if !ok {
		return PackageResponse{}, fmt.Errorf("worker %s not found", workerID)
	}
	workerLibDir := srv.PkgStore.WorkerLibDir(workerID)

	// 2. Load the bundle's manifest for repository configuration.
	bundlePaths := srv.BundlePaths(appID, worker.BundleID)
	manifestPath := filepath.Join(bundlePaths.Base, "manifest.json")
	m, err := manifest.Read(manifestPath)
	if err != nil {
		return PackageResponse{}, fmt.Errorf("read manifest: %w", err)
	}

	// 3. Create staging directory on the store filesystem.
	stagingDir, err := srv.PkgStore.CreateStagingDir()
	if err != nil {
		return PackageResponse{}, err
	}
	defer srv.PkgStore.CleanupStagingDir(stagingDir) //nolint:errcheck

	// 4. Ensure pak and by-builder are cached.
	bsp := srv.Config.Storage.BundleServerPath
	pakPath, err := pakcache.EnsureInstalled(
		ctx, srv.Backend, srv.Config.Docker.Image,
		srv.Config.Docker.PakVersion,
		filepath.Join(bsp, ".pak-cache"))
	if err != nil {
		return PackageResponse{}, fmt.Errorf("ensure pak: %w", err)
	}
	builderPath, err := buildercache.EnsureCached(
		filepath.Join(bsp, ".by-builder-cache"), srv.Version)
	if err != nil {
		return PackageResponse{}, fmt.Errorf("ensure by-builder: %w", err)
	}

	// 5. Run the four-phase install in a build container.
	_, err = srv.runRuntimeInstall(ctx, runtimeInstallParams{
		AppID:        appID,
		WorkerID:     workerID,
		Ref:          req.Name,
		PakPath:      pakPath,
		BuilderPath:  builderPath,
		StagingDir:   stagingDir,
		WorkerLibDir: workerLibDir,
		StoreRoot:    srv.PkgStore.Root(),
		Platform:     srv.PkgStore.Platform(),
		Image:        srv.Config.Docker.Image,
		Repositories: m.Repositories,
	})
	if err != nil {
		return PackageResponse{}, err
	}

	// 6. Check for version conflicts using the worker's package manifest.
	storeManifestPath := filepath.Join(stagingDir, "store-manifest.json")
	workerManifest, err := pkgstore.ReadPackageManifest(workerLibDir)
	if err != nil {
		return PackageResponse{}, fmt.Errorf("read package manifest: %w", err)
	}
	conflict, conflictPkg, err := detectConflict(
		storeManifestPath, workerManifest, req.LoadedNamespaces)
	if err != nil {
		return PackageResponse{}, fmt.Errorf("conflict check: %w", err)
	}

	if conflict {
		slog.Info("runtime install: version conflict detected",
			"worker_id", workerID, "package", conflictPkg)
		return srv.handleTransfer(ctx, appID, workerID, storeManifestPath, nil)
	}

	// 7. No conflict — hardlink new packages from store/staging into /lib.
	if err := srv.linkNewPackages(
		storeManifestPath, workerLibDir,
	); err != nil {
		return PackageResponse{}, fmt.Errorf("link packages: %w", err)
	}

	return PackageResponse{
		Status:  "ok",
		Message: fmt.Sprintf("installed %s", req.Name),
	}, nil
}

// runRuntimeInstall launches a build container that runs the four-phase
// store-aware install with the worker's existing library as a reference.
func (srv *Server) runRuntimeInstall(
	ctx context.Context, p runtimeInstallParams,
) (backend.BuildResult, error) {

	reposJSON, _ := json.Marshal(p.Repositories)

	rScript := `
library(pak, lib.loc = "/pak")
Sys.setenv(PKG_CACHE_DIR = "/pak-cache")

# ── Configure repositories ───────────────────────────────────────
repos_json <- Sys.getenv("REPOS_JSON")
if (nzchar(repos_json)) {
  repos <- jsonlite::fromJSON(repos_json)
  repo_urls <- setNames(repos$URL, repos$Name)

  # Platform URL transformation (same as build flow).
  repo_urls <- vapply(repo_urls, function(url) {
    if (grepl("p3m\\.dev|packagemanager\\.posit\\.co", url) &&
        !grepl("__linux__", url)) {
      os_rel <- readLines("/etc/os-release")
      cn <- sub("^VERSION_CODENAME=", "",
                grep("^VERSION_CODENAME=", os_rel, value = TRUE))
      url <- sub("(/cran/|/bioc/)",
                 paste0("\\1__linux__/", cn, "/"), url)
    }
    url
  }, "")
  options(repos = repo_urls)
}

ref <- Sys.getenv("PKG_REF")
staging <- Sys.getenv("STAGING_DIR")

# ── Phase 1: Resolve against existing library ────────────────────
pak::lockfile_create(
  ref,
  lockfile = file.path(staging, "pak.lock"),
  lib = c(staging, "/worker-lib")
)

# ── Phase 2: Pre-populate staging from store + worker library ────
rc <- system2("/tools/by-builder", c(
  "store", "populate",
  "--lockfile", file.path(staging, "pak.lock"),
  "--lib", staging,
  "--store", "/store",
  "--reference-lib", "/worker-lib",
  "--runtime"
))
if (rc != 0L) {
  message("WARNING: store populate failed (exit ", rc,
          "); falling back to full install")
}

# ── Phase 3: Install store misses ────────────────────────────────
pak::lockfile_install(
  file.path(staging, "pak.lock"),
  lib = staging
)

# ── Phase 4: Ingest newly installed packages into store ──────────
rc <- system2("/tools/by-builder", c(
  "store", "ingest",
  "--lockfile", file.path(staging, "pak.lock"),
  "--lib", staging,
  "--store", "/store",
  "--reference-lib", "/worker-lib"
))
if (rc != 0L) {
  stop("store ingest failed (exit ", rc,
       "); store-manifest.json was not written")
}
`

	spec := backend.BuildSpec{
		AppID:    p.AppID,
		BundleID: "runtime-" + p.WorkerID + "-" + uuid.New().String()[:8],
		Image:    p.Image,
		Cmd:      []string{"R", "--vanilla", "-e", rScript},
		Mounts: []backend.MountEntry{
			{Source: p.WorkerLibDir, Target: "/worker-lib", ReadOnly: true},
			{Source: p.StagingDir, Target: "/staging", ReadOnly: false},
			{Source: p.PakPath, Target: "/pak", ReadOnly: true},
			{Source: filepath.Join(srv.Config.Storage.BundleServerPath, ".pak-dl-cache"),
				Target: "/pak-cache", ReadOnly: false},
			{Source: p.StoreRoot, Target: "/store", ReadOnly: false},
			{Source: p.BuilderPath, Target: "/tools/by-builder", ReadOnly: true},
		},
		Env: []string{
			"PKG_REF=" + p.Ref,
			"REPOS_JSON=" + string(reposJSON),
			"STAGING_DIR=/staging",
		},
		Labels: map[string]string{
			"dev.blockyard/managed":   "true",
			"dev.blockyard/role":      "runtime-install",
			"dev.blockyard/app-id":    p.AppID,
			"dev.blockyard/worker-id": p.WorkerID,
		},
	}

	result, err := srv.Backend.Build(ctx, spec)
	if err != nil {
		return backend.BuildResult{}, fmt.Errorf("runtime install: %w", err)
	}
	if !result.Success {
		return backend.BuildResult{}, fmt.Errorf("runtime install failed (exit %d): %s",
			result.ExitCode, lastLines(result.Logs, 10))
	}
	return result, nil
}

// linkNewPackages hardlinks newly resolved packages from the store into
// the worker's /lib. Packages with a different compound ref (source or
// config hash changed) are replaced.
func (srv *Server) linkNewPackages(
	storeManifestPath, workerLibDir string,
) error {
	newManifest, err := pkgstore.ReadStoreManifest(storeManifestPath)
	if err != nil {
		return err
	}

	workerManifest, _ := pkgstore.ReadPackageManifest(workerLibDir)
	if workerManifest == nil {
		workerManifest = make(map[string]string)
	}

	newEntries := make(map[string]string)

	for pkg, ref := range newManifest {
		sourceHash, configHash, err := pkgstore.SplitStoreRef(ref)
		if err != nil {
			return fmt.Errorf("bad store ref for %s: %w", pkg, err)
		}

		storePath := srv.PkgStore.Path(pkg, sourceHash, configHash)
		if !dirExists(storePath) {
			return fmt.Errorf(
				"package %s not in store at %s", pkg, ref)
		}

		destPath := filepath.Join(workerLibDir, pkg)
		if dirExists(destPath) {
			if workerManifest[pkg] == ref {
				continue
			}
			os.RemoveAll(destPath)
		}

		out, err := exec.Command("cp", "-al", storePath, destPath).CombinedOutput()
		if err != nil {
			return fmt.Errorf("link %s: %s: %w", pkg, out, err)
		}

		srv.PkgStore.Touch(pkg, sourceHash, configHash)
		newEntries[pkg] = ref
	}

	if len(newEntries) > 0 {
		if err := pkgstore.UpdatePackageManifest(workerLibDir, newEntries); err != nil {
			return fmt.Errorf("update package manifest: %w", err)
		}
	}
	return nil
}

// lastLines returns the last n lines of a string.
func lastLines(s string, n int) string {
	lines := make([]string, 0, n)
	start := len(s)
	for i := 0; i < n && start > 0; i++ {
		end := start
		start--
		for start > 0 && s[start] != '\n' {
			start--
		}
		if start > 0 {
			start++ // skip the newline
		}
		lines = append([]string{s[start:end]}, lines...)
		if start > 0 {
			start-- // step back over the newline for the next iteration
		}
	}
	result := ""
	for i, l := range lines {
		if i > 0 {
			result += "\n"
		}
		result += l
	}
	return result
}
