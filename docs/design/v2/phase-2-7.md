# Phase 2-7: Runtime Package Assembly & Dependency Refresh

Extends the package store with runtime capabilities: live package
installation for running workers and dependency refresh for unpinned
deployments. Both use the same store-aware build flow from phase 2-6,
differing only in resolution context and trigger.

Depends on phase 2-6 (package store, worker library assembly,
store-manifest output).

See [dep-mgmt.md](../dep-mgmt.md) for the full runtime assembly flow,
container transfer protocol, refresh mechanics, and design rationale.
This document covers how to build them.

## Deliverables

1. **Runtime package API** (`POST /api/v1/packages`) — blocking endpoint
   that accepts a package name (or pkgdepends ref) and the R session's
   loaded namespaces. Resolves against the worker's existing library
   (lazy upgrade policy). Three outcomes: success (store hit → instant
   hardlink), success (store miss → install + ingest + hardlink), or
   transfer (version conflict requiring new container).
2. **Worker HMAC authentication** — worker tokens for in-container API
   access. Generated at spawn time, injected as an environment variable.
   The packages endpoint validates the token.
3. **Staging directory flow** — staging directories on the store's
   filesystem (`/store/.staging/{uuid}/`) so store hits can be hardlinked
   in for pak to see, and newly installed packages can be atomically
   `rename()`'d into the store.
4. **Multi-library resolution** — runtime `lockfile_create()` receives
   the worker's `/lib` as a reference library, so the solver sees what's
   installed and applies the lazy upgrade policy. `lockfile_install()`
   installs into the staging directory only.
5. **Version conflict detection** — after resolution, compare the
   store-manifest's refs against the R session's loaded namespaces. If a
   dependency must change version and is already loaded, return
   `"transfer"` status.
6. **Container transfer** — when a version conflict requires a new
   container: the R code (blockr) serializes board state to a
   transfer path (atomic rename). Server watches for the file, spawns a
   new worker with the updated library, reroutes traffic, drains the old
   worker. Non-blockr apps get a hard restart.
7. **Dependency refresh API** (`POST /api/v1/apps/{id}/refresh`) — for
   unpinned deployments only. Re-resolves dependencies using the original
   unpinned manifest. Produces a new store-manifest, reassembles the
   worker library. Also persists the new pak.lock as audit artifact.
8. **Refresh triggers** — manual (CLI command, dashboard button),
   scheduled (per-app cron), and optionally on cold start.
9. **Refresh rollback** — previous store-manifests are retained. Rollback
   reassembles the library from the prior store-manifest (instant — store
   is append-only).

---

## Step 1: Worker HMAC authentication

Workers need to call the packages endpoint from inside the container.
The existing session token mechanism (HMAC-SHA256 via `auth.SigningKey`)
is reused with a worker-scoped claim.

### Token generation

At spawn time, generate a worker token and inject it as an environment
variable:

```go
// internal/proxy/coldstart.go

func workerToken(signingKey *auth.SigningKey, appID, workerID string) (string, error) {
    claims := &auth.SessionTokenClaims{
        Sub: "worker:" + workerID,
        App: appID,
        Wid: workerID,
        Iat: time.Now().Unix(),
        Exp: time.Now().Add(365 * 24 * time.Hour).Unix(), // long-lived — revoked on eviction
    }
    return auth.EncodeSessionToken(claims, signingKey)
}
```

The token is long-lived because the worker's lifetime is its validity
window — when the worker is evicted, the token is meaningless. The
`Sub` prefix `worker:` distinguishes worker tokens from user session
tokens.

### WorkerSpec.Env update

Add the token to the worker's environment:

```go
env["BLOCKYARD_WORKER_TOKEN"] = token
env["BLOCKYARD_API_URL"] = srv.InternalAPIURL() // e.g., http://host.docker.internal:3939
```

`BLOCKYARD_API_URL` is the server's internal URL reachable from inside
containers. This already exists for credential exchange — reused here.

### Middleware

The packages endpoint validates the worker token and extracts the
worker ID:

```go
// internal/api/workerauth.go

func WorkerAuth(signingKey *auth.SigningKey) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            token := r.Header.Get("Authorization")
            if token == "" {
                http.Error(w, "missing worker token", http.StatusUnauthorized)
                return
            }
            token = strings.TrimPrefix(token, "Bearer ")

            claims, err := auth.DecodeSessionToken(token, signingKey)
            if err != nil {
                http.Error(w, "invalid worker token", http.StatusUnauthorized)
                return
            }
            if !strings.HasPrefix(claims.Sub, "worker:") {
                http.Error(w, "not a worker token", http.StatusForbidden)
                return
            }

            ctx := context.WithValue(r.Context(), workerIDKey, claims.Wid)
            ctx = context.WithValue(ctx, appIDKey, claims.App)
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}
```

---

## Step 2: Runtime package API

### Types

```go
// internal/api/packages.go

type PackageRequest struct {
    Name             string   `json:"name"`              // package name or pkgdepends ref
    LoadedNamespaces []string `json:"loaded_namespaces"` // from loadedNamespaces() in R
}

type PackageResponse struct {
    Status       string `json:"status"`                  // "ok", "transfer", "error"
    Message      string `json:"message,omitempty"`
    TransferPath string `json:"transfer_path,omitempty"` // set when status == "transfer"
}
```

### Handler

```go
// internal/api/packages.go

func PostPackages(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        workerID := r.Context().Value(workerIDKey).(string)
        appID := r.Context().Value(appIDKey).(string)

        var req PackageRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
            writeJSON(w, http.StatusBadRequest,
                PackageResponse{Status: "error", Message: "invalid request"})
            return
        }

        result, err := srv.InstallPackage(r.Context(), appID, workerID, req)
        if err != nil {
            writeJSON(w, http.StatusInternalServerError,
                PackageResponse{Status: "error", Message: err.Error()})
            return
        }

        writeJSON(w, http.StatusOK, result)
    }
}
```

### Route registration

```go
// internal/api/routes.go

r.Route("/api/v1/packages", func(r chi.Router) {
    r.Use(api.WorkerAuth(srv.SigningKey))
    r.Post("/", api.PostPackages(srv))
})
```

---

## Step 3: Staging directory flow

Runtime package installation uses a staging directory on the store's
filesystem. This is critical: store hits can be hardlinked into the
staging dir for pak to see as installed, and newly installed packages
can be atomically `rename()`'d into the store — no cross-filesystem
copy.

### Store staging operations

```go
// internal/pkgstore/staging.go

// CreateStagingDir creates a staging directory under the store root.
func (s *Store) CreateStagingDir() (string, error) {
    dir := filepath.Join(s.root, ".staging", uuid.New().String())
    if err := os.MkdirAll(dir, 0o755); err != nil {
        return "", fmt.Errorf("create staging dir: %w", err)
    }
    return dir, nil
}

// CleanupStagingDir removes a staging directory.
func (s *Store) CleanupStagingDir(dir string) error {
    return os.RemoveAll(dir)
}
```

### Startup cleanup

Clean up orphaned staging directories from previous runs (same pattern
as worker library cleanup in phase 2-6):

```go
// At server startup:
stagingDir := filepath.Join(srv.PkgStore.Root(), ".staging")
entries, _ := os.ReadDir(stagingDir)
for _, e := range entries {
    if e.IsDir() {
        os.RemoveAll(filepath.Join(stagingDir, e.Name()))
    }
}
```

---

## Step 4: InstallPackage server method

The core orchestration. Uses the same four-phase store-aware flow as
the build pipeline (phase 2-6), differing in resolution context:
the worker's existing `/lib` is a reference library so pak sees
what's installed.

Runtime installs are serialized per-worker via `workerInstallMu` — a
per-worker mutex held for the entire install. This avoids races on the
worker's `.packages.json` (read-modify-write), prevents two concurrent
`lockfile_create()` calls from resolving against the same library state,
and ensures the library is consistent for conflict detection.

```go
// internal/server/packages.go

func (srv *Server) InstallPackage(
    ctx context.Context,
    appID, workerID string,
    req api.PackageRequest,
) (api.PackageResponse, error) {

    // Serialize runtime installs per-worker. A worker can have multiple
    // sessions (max_sessions_per_worker), and two sessions requesting
    // different packages simultaneously would race on the library state,
    // .packages.json, and conflict detection.
    mu := srv.workerInstallMu(workerID)
    mu.Lock()
    defer mu.Unlock()

    // 1. Look up the worker and its library path.
    worker, ok := srv.Workers.Get(workerID)
    if !ok {
        return api.PackageResponse{}, fmt.Errorf("worker %s not found", workerID)
    }
    workerLibDir := srv.PkgStore.WorkerLibDir(workerID)

    // 2. Load the bundle's manifest for repository configuration.
    bundlePaths := srv.BundlePaths(appID, worker.BundleID)
    manifestPath := filepath.Join(bundlePaths.Base, "manifest.json")
    m, err := manifest.Read(manifestPath)
    if err != nil {
        return api.PackageResponse{}, fmt.Errorf("read manifest: %w", err)
    }

    // 3. Create staging directory on the store filesystem.
    stagingDir, err := srv.PkgStore.CreateStagingDir()
    if err != nil {
        return api.PackageResponse{}, err
    }
    defer srv.PkgStore.CleanupStagingDir(stagingDir)

    // 4. Ensure pak and by-builder are cached.
    pakPath, err := pakcache.EnsureInstalled(
        ctx, srv.Backend, srv.Config.Docker.Image,
        srv.Config.Docker.PakVersion, srv.Config.Docker.PakCachePath())
    if err != nil {
        return api.PackageResponse{}, fmt.Errorf("ensure pak: %w", err)
    }
    builderPath, err := buildercache.EnsureCached(
        srv.Config.Docker.BuilderCachePath(), srv.Config.Docker.BuilderVersion)
    if err != nil {
        return api.PackageResponse{}, fmt.Errorf("ensure by-builder: %w", err)
    }

    // 5. Run the four-phase install in a build container.
    result, err := srv.runRuntimeInstall(ctx, runtimeInstallParams{
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
        return api.PackageResponse{}, err
    }

    // 6. Check for version conflicts using the worker's package manifest.
    storeManifestPath := filepath.Join(stagingDir, "store-manifest.json")
    workerManifest, err := pkgstore.ReadPackageManifest(workerLibDir)
    if err != nil {
        return api.PackageResponse{}, fmt.Errorf("read package manifest: %w", err)
    }
    conflict, conflictPkg, err := detectConflict(
        storeManifestPath, workerManifest, req.LoadedNamespaces)
    if err != nil {
        return api.PackageResponse{}, fmt.Errorf("conflict check: %w", err)
    }

    if conflict {
        return srv.handleTransfer(ctx, appID, workerID, storeManifestPath, result)
    }

    // 7. No conflict — hardlink new packages from store/staging into /lib.
    if err := srv.linkNewPackages(
        storeManifestPath, workerLibDir,
    ); err != nil {
        return api.PackageResponse{}, fmt.Errorf("link packages: %w", err)
    }

    return api.PackageResponse{
        Status:  "ok",
        Message: fmt.Sprintf("installed %s", req.Name),
    }, nil
}
```

### runtimeInstallParams

```go
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
```

### Server struct additions

```go
// internal/server/server.go

type Server struct {
    // ... existing fields from phase 2-6 ...

    // Per-worker mutex for runtime package installs. Serializes
    // installs to the same worker to avoid races on .packages.json,
    // library state, and conflict detection.
    installMus sync.Map // workerID → *sync.Mutex
}

// workerInstallMu returns a per-worker mutex, creating one if needed.
func (srv *Server) workerInstallMu(workerID string) *sync.Mutex {
    v, _ := srv.installMus.LoadOrStore(workerID, &sync.Mutex{})
    return v.(*sync.Mutex)
}
```

The `sync.Map` entry is cleaned up in `EvictWorker` alongside the
worker library and transfer directory.

---

## Step 5: Runtime R script

The R script running inside the build container implements the
four-phase store-aware flow with the worker's existing library as
a reference. R handles phases 1 and 3 (pak API calls); the
`by-builder` binary handles phases 2 and 4 (store operations).

The key difference from the build flow (phase 2-6):
`lockfile_create()` receives the worker library as a reference so
the solver sees what's already installed and applies the lazy upgrade
policy (`upgrade = FALSE`).

```r
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

ref <- Sys.getenv("PKG_REF")  # e.g., "DT" or "owner/repo"
staging <- Sys.getenv("STAGING_DIR")  # /staging

# ── Phase 1: Resolve against existing library ────────────────────
# The worker's /lib is mounted read-only as a reference library.
# pak sees installed packages and applies upgrade = FALSE.
pak::lockfile_create(
  ref,
  lockfile = file.path(staging, "pak.lock"),
  lib = c(staging, "/worker-lib")
)

# ── Phase 2: Check store for NEW entries ─────────────────────────
# by-builder skips packages already in the worker library
# (--reference-lib), checks the store, and hard-links hits into
# the staging directory.
system2("/tools/by-builder", c(
  "store", "populate",
  "--lockfile", file.path(staging, "pak.lock"),
  "--lib", staging,
  "--store", "/store",
  "--reference-lib", "/worker-lib"
))

# ── Phase 3: Install store misses ────────────────────────────────
pak::lockfile_install(
  file.path(staging, "pak.lock"),
  lib = c(staging, "/worker-lib")
)
# Installs into staging only. /worker-lib is reference — not modified.

# ── Phase 4: Ingest newly installed packages into store ──────────
# by-builder ingests new packages from staging into the store,
# skipping packages from the worker library.
system2("/tools/by-builder", c(
  "store", "ingest",
  "--lockfile", file.path(staging, "pak.lock"),
  "--lib", staging,
  "--store", "/store",
  "--reference-lib", "/worker-lib"
))
```

### Container mounts

```
/worker-lib        (ro)  ← worker's assembled library (reference only)
/staging           (rw)  ← staging directory (on store filesystem)
/pak               (ro)  ← cached pak package
/pak-cache         (rw)  ← persistent pak download cache
/store             (rw)  ← package store (for ingestion)
/tools/by-builder  (ro)  ← cached by-builder binary
```

### Go-side container launch

```go
// internal/server/packages.go

func (srv *Server) runRuntimeInstall(
    ctx context.Context, p runtimeInstallParams,
) (*backend.BuildResult, error) {

    reposJSON, _ := json.Marshal(p.Repositories)

    rScript := `... (runtime R script from above) ...`

    spec := backend.BuildSpec{
        AppID:    p.AppID,
        BundleID: "runtime-" + p.WorkerID + "-" + uuid.New().String()[:8],
        Image:    p.Image,
        Cmd:      []string{"R", "--vanilla", "-e", rScript},
        Mounts: []backend.MountEntry{
            {Source: p.WorkerLibDir, Target: "/worker-lib", ReadOnly: true},
            {Source: p.StagingDir, Target: "/staging", ReadOnly: false},
            {Source: p.PakPath, Target: "/pak", ReadOnly: true},
            {Source: srv.DLCachePath(), Target: "/pak-cache", ReadOnly: false},
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
        return nil, fmt.Errorf("runtime install: %w", err)
    }
    if !result.Success {
        return nil, fmt.Errorf("runtime install failed (exit %d): %s",
            result.ExitCode, lastLines(result.Logs, 10))
    }
    return result, nil
}
```

---

## Step 6: Version conflict detection

After resolution, compare the new store-manifest (written by
`by-builder store ingest` to the staging directory) against the worker's
current package manifest (`.packages.json`). The manifest is a
`{package → "sourceHash/configHash"}` map written at library assembly
time (phase 2-6) and updated on live installs (step 7). If any loaded
namespace has a different compound ref in the new store-manifest, R
cannot unload and reload it in the same session — that's a version
conflict.

Using compound store refs instead of version strings catches every
meaningful change: version bumps (different source hash), PPM rebuilds
(different sha256 in source hash), AND LinkingTo ABI changes (same
source hash but different config hash — e.g., sf compiled against a
new Rcpp version).

```go
// internal/server/conflict.go

// detectConflict checks whether the new store-manifest conflicts with
// the R session's loaded namespaces by comparing compound store refs
// from the worker's .packages.json manifest against those in the
// store-manifest (written by `by-builder store ingest`). The compound
// ref encodes both the source identity (version/sha256) and the ABI
// configuration (LinkingTo store keys), catching both version changes
// and LinkingTo recompilation needs.
func detectConflict(
    storeManifestPath string,
    workerManifest map[string]string,
    loadedNamespaces []string,
) (conflict bool, pkg string, err error) {
    newRefs, err := pkgstore.ReadStoreManifest(storeManifestPath)
    if err != nil {
        return false, "", err
    }

    for _, ns := range loadedNamespaces {
        currentRef, installed := workerManifest[ns]
        if !installed {
            continue
        }
        newRef, inNewManifest := newRefs[ns]
        if !inNewManifest {
            continue
        }
        if currentRef != newRef {
            return true, ns, nil
        }
    }
    return false, "", nil
}
```

Packages that are installed but not loaded can be updated in place —
the hardlink in `/lib` is replaced with the correct version/config
(see step 7).

---

## Step 7: Linking new packages into the worker library

When there's no version conflict, newly resolved packages are
hardlinked from the store (or staging directory) into the worker's
`/lib`. Packages that are already in `/lib` but have a different
compound ref (source or config hash changed) are replaced — this
handles the case where a LinkingTo dependency changed and the
existing build has stale ABI.

```go
// internal/server/packages.go

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
```

---

## Step 8: Container transfer

When a version conflict is detected, the server initiates a container
transfer. For blockr apps, the board state is serialized and restored
in a new container. For non-blockr apps, the session is lost (hard
restart).

### Transfer directory

```go
// internal/server/transfer.go

// TransferDir returns the host-side transfer directory for a worker.
func (srv *Server) TransferDir(workerID string) string {
    return filepath.Join(srv.Config.Docker.BundleServerPath,
        ".transfers", workerID)
}
```

### handleTransfer

```go
func (srv *Server) handleTransfer(
    ctx context.Context,
    appID, workerID, storeManifestPath string,
    buildResult *backend.BuildResult,
) (api.PackageResponse, error) {

    // The transfer directory was pre-created and mounted into the
    // worker at spawn time (phase 2-6). It's already at /transfer
    // inside the container.
    transferDir := srv.TransferDir(workerID)

    // Copy the store-manifest to the transfer directory before returning.
    // The staging directory (where the store-manifest lives) is cleaned
    // up by the caller's defer — completeTransfer reads it from here.
    transferManifest := filepath.Join(transferDir, "store-manifest.json")
    if err := copyFile(storeManifestPath, transferManifest); err != nil {
        return api.PackageResponse{},
            fmt.Errorf("copy store-manifest to transfer dir: %w", err)
    }

    // Start watching for the board state file in a background goroutine.
    go srv.watchTransfer(ctx, appID, workerID, transferManifest, transferDir)

    // Return the container-side path. The worker's /transfer mount
    // maps to transferDir on the host. The R session writes board.json
    // to this path; the server watches the host-side path.
    return api.PackageResponse{
        Status:       "transfer",
        Message:      "version conflict — container transfer required",
        TransferPath: "/transfer",
    }, nil
}
```

### Transfer completion (background goroutine)

After the API returns `"transfer"`, the server starts watching for
the board state file. This runs as a background goroutine so the
HTTP response is sent immediately.

```go
// internal/server/transfer.go

func (srv *Server) watchTransfer(
    ctx context.Context,
    appID, workerID, storeManifestPath, transferDir string,
) {
    boardPath := filepath.Join(transferDir, "board.json")
    timeout := 30 * time.Second
    pollInterval := 100 * time.Millisecond

    deadline := time.Now().Add(timeout)
    for time.Now().Before(deadline) {
        if _, err := os.Stat(boardPath); err == nil {
            // Board state written — proceed with transfer.
            srv.completeTransfer(ctx, appID, workerID,
                storeManifestPath, transferDir)
            return
        }
        time.Sleep(pollInterval)
    }

    // Timeout — abort transfer.
    slog.Error("transfer timeout",
        "worker_id", workerID, "app_id", appID)
    os.RemoveAll(transferDir)
}
```

### completeTransfer

```go
func (srv *Server) completeTransfer(
    ctx context.Context,
    appID, oldWorkerID, storeManifestPath, transferDir string,
) {
    // 1. Read the old worker's package manifest and merge with the new
    //    runtime store-manifest. The runtime store-manifest is PARTIAL —
    //    it only contains packages that were in the staging directory
    //    (new installs + store-linked updates). Unchanged packages from
    //    the original build are only in the old worker's .packages.json.
    //    Without merging, the new worker would be missing most packages.
    oldWorkerLib := srv.PkgStore.WorkerLibDir(oldWorkerID)
    oldManifest, err := pkgstore.ReadPackageManifest(oldWorkerLib)
    if err != nil {
        slog.Error("transfer: read old worker manifest", "error", err)
        return
    }

    newRefs, err := pkgstore.ReadStoreManifest(storeManifestPath)
    if err != nil {
        slog.Error("transfer: read store-manifest", "error", err)
        return
    }

    // Start with the old worker's full package set, then overlay
    // new/changed entries from the runtime install.
    mergedManifest := make(map[string]string, len(oldManifest))
    for pkg, ref := range oldManifest {
        mergedManifest[pkg] = ref
    }
    for pkg, ref := range newRefs {
        mergedManifest[pkg] = ref
    }

    newWorkerID := uuid.New().String()
    newLibDir := srv.PkgStore.WorkerLibDir(newWorkerID)
    missing, err := srv.PkgStore.AssembleLibrary(newLibDir, mergedManifest)
    if err != nil {
        slog.Error("transfer: assemble library", "error", err)
        return
    }
    if len(missing) > 0 {
        slog.Warn("transfer: missing store entries",
            "worker_id", newWorkerID, "missing", missing)
    }

    // 2. Spawn new worker with updated library. Mount the old worker's
    // transfer dir (containing board.json) read-only at /transfer.
    spec := srv.buildTransferWorkerSpec(appID, newWorkerID, newLibDir, transferDir)
    if err := srv.Backend.SpawnWorker(ctx, spec); err != nil {
        slog.Error("transfer: spawn worker", "error", err)
        return
    }

    // 3. Wait for new worker to become healthy.
    if err := srv.waitHealthy(ctx, newWorkerID); err != nil {
        slog.Error("transfer: worker not healthy", "error", err)
        return
    }

    // 4. Reroute traffic from old worker to new worker.
    srv.LoadBalancer.Transfer(oldWorkerID, newWorkerID)

    // 5. Drain and stop old worker.
    srv.EvictWorker(ctx, oldWorkerID)

    // 6. Clean up transfer directory.
    os.RemoveAll(transferDir)

    slog.Info("transfer complete",
        "app_id", appID,
        "old_worker", oldWorkerID,
        "new_worker", newWorkerID)
}
```

### Worker mount for transfer

Every worker has a `/transfer` mount pre-created at spawn time
(phase 2-6). For Worker B (the new worker receiving a board state
transfer), the old worker's transfer directory is mounted instead of
the new worker's empty one, and `BLOCKYARD_TRANSFER_PATH` is set:

```go
func (srv *Server) buildTransferWorkerSpec(
    appID, workerID, libDir, oldTransferDir string,
) backend.WorkerSpec {
    spec := srv.defaultWorkerSpec(appID, workerID, libDir)

    if oldTransferDir != "" {
        // Override the default transfer mount with the old worker's
        // transfer directory (read-only — Worker B only reads board.json).
        spec.TransferDir = oldTransferDir
        spec.Env["BLOCKYARD_TRANSFER_PATH"] = "/transfer/board.json"
    }

    return spec
}
```

### R-side (blockr) transfer flow

The R code in the worker uses the packages API and handles the
transfer response:

```r
# Called by blockr when a new package is needed at runtime.
request_package <- function(pkg_name) {
  token <- Sys.getenv("BLOCKYARD_WORKER_TOKEN")
  api_url <- Sys.getenv("BLOCKYARD_API_URL")

  body <- list(
    name = pkg_name,
    loaded_namespaces = loadedNamespaces()
  )

  resp <- httr2::request(paste0(api_url, "/api/v1/packages")) |>
    httr2::req_headers(Authorization = paste("Bearer", token)) |>
    httr2::req_body_json(body) |>
    httr2::req_perform()

  result <- httr2::resp_body_json(resp)

  if (result$status == "ok") {
    # Package installed — reload .libPaths() to pick it up.
    .libPaths(.libPaths())
    return(invisible(TRUE))
  }

  if (result$status == "transfer") {
    # Version conflict — serialize board and let server handle transfer.
    # transfer_path is a container-side path (/transfer) — the directory
    # was pre-mounted rw at spawn time. The server watches the host-side
    # path for board.json to appear.
    transfer_path <- result$transfer_path
    board_json <- jsonlite::toJSON(board_state(), auto_unbox = TRUE)
    tmp <- paste0(file.path(transfer_path, "board.json"), ".tmp")
    writeLines(board_json, tmp)
    file.rename(tmp, file.path(transfer_path, "board.json"))

    # Block — this session is being replaced.
    Sys.sleep(Inf)
  }

  stop("Package install failed: ", result$message)
}
```

### Board restore on startup

The new worker checks for a transfer file on startup:

```r
transfer_path <- Sys.getenv("BLOCKYARD_TRANSFER_PATH")
if (nzchar(transfer_path) && file.exists(transfer_path)) {
  board <- jsonlite::fromJSON(transfer_path)
  restore_board(board)
}
```

---

## Step 9: Dependency refresh

Refresh re-runs the build pipeline for an existing bundle without
re-uploading code. Only available for unpinned deployments.

### Refresh API

```go
// internal/api/refresh.go

func PostRefresh(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        appID := chi.URLParam(r, "id")
        caller := auth.CallerFromContext(r.Context())

        app, err := resolveAppRelation(srv, w, caller, appID)
        if err != nil {
            return
        }

        // Only unpinned deployments can be refreshed.
        manifestPath := filepath.Join(
            srv.BundlePaths(app.ID, app.ActiveBundleID).Base,
            "manifest.json")
        m, err := manifest.Read(manifestPath)
        if err != nil {
            writeJSON(w, http.StatusInternalServerError,
                map[string]string{"message": "read manifest: " + err.Error()})
            return
        }
        if m.IsPinned() {
            writeJSON(w, http.StatusConflict,
                map[string]string{
                    "message": "app was deployed with pinned dependencies; " +
                        "redeploy to update",
                })
            return
        }

        // Start refresh as a background task.
        taskID := srv.Tasks.Create(app.ID, "refresh")
        go srv.RunRefresh(context.Background(), app, m, taskID)

        writeJSON(w, http.StatusAccepted, map[string]string{
            "task_id": taskID,
            "message": "refresh started",
        })
    }
}
```

### Route registration

```go
r.Post("/api/v1/apps/{id}/refresh", api.PostRefresh(srv))
```

### RunRefresh

```go
// internal/server/refresh.go

// RunRefresh re-resolves dependencies for an unpinned deployment.
// Returns true if a new worker was spawned (dependencies changed).
func (srv *Server) RunRefresh(
    ctx context.Context,
    app *db.App,
    m *manifest.Manifest,
    taskID string,
) bool {
    sender := srv.Tasks.Sender(taskID)
    defer srv.Tasks.Complete(taskID)

    sender.Write("refreshing dependencies...")

    // 1. Ensure pak and by-builder are cached.
    pakPath, err := pakcache.EnsureInstalled(
        ctx, srv.Backend, srv.Config.Docker.Image,
        srv.Config.Docker.PakVersion, srv.Config.Docker.PakCachePath())
    if err != nil {
        srv.Tasks.Fail(taskID, fmt.Errorf("ensure pak: %w", err))
        return false
    }
    builderPath, err := buildercache.EnsureCached(
        srv.Config.Docker.BuilderCachePath(), srv.Config.Docker.BuilderVersion)
    if err != nil {
        srv.Tasks.Fail(taskID, fmt.Errorf("ensure by-builder: %w", err))
        return false
    }

    // 2. Get the bundle's unpacked path (contains DESCRIPTION / scripts).
    bundlePaths := srv.BundlePaths(app.ID, app.ActiveBundleID)

    // 3. Run the standard build flow using the original unpinned manifest.
    //    This re-resolves dependencies: Remotes against current upstream,
    //    CRAN packages against the manifest's repository URLs.
    buildUUID := uuid.New().String()
    dlCachePath := filepath.Join(srv.Config.Docker.BundleServerPath,
        ".pak-dl-cache")
    os.MkdirAll(dlCachePath, 0o755)

    spec := backend.BuildSpec{
        AppID:    app.ID,
        BundleID: "refresh-" + buildUUID[:8],
        Image:    srv.Config.Docker.Image,
        Cmd:      buildCommand(),
        Mounts: buildMounts(
            pakPath, bundlePaths.Unpacked,
            srv.PkgStore.Root(), dlCachePath, builderPath),
        Env: []string{"BUILD_UUID=" + buildUUID},
        Labels: map[string]string{
            "dev.blockyard/managed": "true",
            "dev.blockyard/role":    "refresh",
            "dev.blockyard/app-id": app.ID,
        },
        LogWriter: func(line string) { sender.Write(line) },
    }

    result, err := srv.Backend.Build(ctx, spec)
    if err != nil {
        srv.Tasks.Fail(taskID, fmt.Errorf("refresh build: %w", err))
        return false
    }
    if !result.Success {
        srv.Tasks.Fail(taskID,
            fmt.Errorf("refresh failed (exit %d)", result.ExitCode))
        return false
    }

    // 4. Extract store-manifest (primary) and pak.lock (audit) from build dir.
    buildDir := filepath.Join(srv.PkgStore.Root(), ".builds", buildUUID)
    defer os.RemoveAll(buildDir)

    newManifestSrc := filepath.Join(buildDir, "store-manifest.json")
    newManifestDst := filepath.Join(bundlePaths.Base, "store-manifest.json")

    // Also persist pak.lock as a debug/audit artifact (never re-parsed).
    newLockfileSrc := filepath.Join(buildDir, "pak.lock")
    newLockfileDst := filepath.Join(bundlePaths.Base, "pak.lock")
    if fileExists(newLockfileSrc) {
        copyFile(newLockfileSrc, newLockfileDst)
    }

    // 5. Archive previous store-manifest for rollback.
    prevManifest := filepath.Join(bundlePaths.Base, "store-manifest.json.prev")
    if fileExists(newManifestDst) {
        copyFile(newManifestDst, prevManifest)
    }

    if err := copyFile(newManifestSrc, newManifestDst); err != nil {
        srv.Tasks.Fail(taskID,
            fmt.Errorf("persist new store-manifest: %w", err))
        return false
    }

    // 6. Check if anything actually changed (map comparison).
    changed, err := storeManifestsChanged(prevManifest, newManifestDst)
    if err != nil {
        slog.Warn("refresh: store-manifest comparison failed, assuming changed",
            "error", err)
        changed = true
    }
    if !changed {
        sender.Write("dependencies unchanged — no action needed")
        return false
    }

    // 7. Graceful drain: spawn new worker, drain old ones.
    sender.Write("dependencies updated — spawning new worker...")
    srv.drainAndReplace(ctx, app, newManifestDst, sender)
    return true
}
```

### drainAndReplace

Graceful drain strategy: spawn a new worker with the updated library,
mark old workers as draining (no new sessions routed to them), and let
existing sessions finish undisturbed. Old workers are evicted when they
have no remaining active sessions. Board serialization / container
transfer is **not** used during refresh — that mechanism is reserved
exclusively for live install conflicts (step 8).

```go
// internal/server/refresh.go

func (srv *Server) drainAndReplace(
    ctx context.Context,
    app *db.App,
    storeManifestPath string,
    sender task.Sender,
) {
    storeManifest, err := pkgstore.ReadStoreManifest(storeManifestPath)
    if err != nil {
        sender.Write("error reading store-manifest: " + err.Error())
        return
    }

    // 1. Spawn a new worker with the updated library.
    newWorkerID := uuid.New().String()
    newLibDir := srv.PkgStore.WorkerLibDir(newWorkerID)
    missing, err := srv.PkgStore.AssembleLibrary(newLibDir, storeManifest)
    if err != nil {
        sender.Write("error assembling library: " + err.Error())
        return
    }
    if len(missing) > 0 {
        sender.Write(fmt.Sprintf("warning: %d packages missing from store", len(missing)))
    }

    spec := srv.defaultWorkerSpec(app.ID, newWorkerID, newLibDir)
    if err := srv.Backend.SpawnWorker(ctx, spec); err != nil {
        sender.Write("error spawning new worker: " + err.Error())
        return
    }

    if err := srv.waitHealthy(ctx, newWorkerID); err != nil {
        sender.Write("new worker not healthy: " + err.Error())
        return
    }

    // 2. Mark old workers as draining — no new sessions routed to them.
    oldWorkers := srv.Workers.ForApp(app.ID)
    for _, oldID := range oldWorkers {
        if oldID == newWorkerID {
            continue
        }
        srv.LoadBalancer.Drain(oldID)
        sender.Write(fmt.Sprintf("draining worker %s", oldID[:8]))
    }

    sender.Write(fmt.Sprintf("new worker %s ready, old workers draining", newWorkerID[:8]))
}
```

The `LoadBalancer.Drain(workerID)` method marks a worker so the load
balancer stops routing **new** sessions to it. Existing sessions
continue on the old worker until they disconnect. The worker eviction
loop (already running in the server's background) periodically checks
drained workers and evicts any that have zero active sessions.

### storeManifestsChanged

Trivial map comparison — the store-manifest is already a
`{package → "sourceHash/configHash"}` map, so no store-key derivation
is needed.

```go
// internal/server/refresh.go

func storeManifestsChanged(oldPath, newPath string) (bool, error) {
    oldManifest, err := pkgstore.ReadStoreManifest(oldPath)
    if err != nil {
        return false, err
    }
    newManifest, err := pkgstore.ReadStoreManifest(newPath)
    if err != nil {
        return false, err
    }
    if len(oldManifest) != len(newManifest) {
        return true, nil
    }
    for pkg, ref := range newManifest {
        if oldManifest[pkg] != ref {
            return true, nil
        }
    }
    return false, nil
}
```

---

## Step 10: Refresh triggers

### Manual

- CLI: `by refresh <app-id>` wraps `POST /api/v1/apps/{id}/refresh`
  (implemented in phase 2-9).
- Dashboard: refresh button on the per-app settings panel (implemented
  in phase 2-8).

### Scheduled (per-app cron)

Add a `refresh_schedule` column to the apps table:

```sql
ALTER TABLE apps ADD COLUMN refresh_schedule TEXT;
-- e.g., "0 3 * * 1" (weekly Monday 3am), or empty string (disabled)
```

The server runs a lightweight scheduler that checks active apps with
a `refresh_schedule` and triggers refresh at the configured times:

```go
// internal/server/scheduler.go

func (srv *Server) runRefreshScheduler(ctx context.Context) {
    ticker := time.NewTicker(1 * time.Minute)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case now := <-ticker.C:
            apps, _ := srv.DB.ListAppsWithRefreshSchedule()
            for _, app := range apps {
                if shouldRun(app.RefreshSchedule, app.LastRefresh, now) {
                    go srv.triggerRefresh(ctx, app)
                }
            }
        }
    }
}
```

The scheduler runs in the server's main goroutine. `shouldRun()`
parses the cron expression and checks whether it fires between
`LastRefresh` and `now`.

### On cold start (optional)

Add a `refresh_on_start` boolean column to the apps table. When set,
the server triggers a refresh before spawning a worker on cold start:

```go
// internal/proxy/coldstart.go

if app.RefreshOnStart && !manifest.IsPinned() {
    spawned := srv.RunRefresh(ctx, app, m, taskID)
    if spawned {
        // Refresh already spawned a worker with updated deps.
        // Skip the normal cold-start spawn to avoid a duplicate.
        return
    }
    // Dependencies unchanged — fall through to normal worker spawn
    // using the existing store-manifest.
}
// Proceed with normal worker spawn.
```

This keeps long-lived scan-mode apps current without manual
intervention. Disabled by default. When refresh detects no changes,
the cold-start path spawns a worker as usual using the existing
store-manifest.

---

## Step 11: Refresh rollback

Each refresh archives the previous store-manifest as
`store-manifest.json.prev`. Rolling back is a library reassembly from
the prior store-manifest — same mechanism as the refresh itself, just
pointing at the old store-manifest.

```go
// internal/server/refresh.go

func (srv *Server) RollbackRefresh(
    ctx context.Context, app *db.App,
) error {
    bundlePaths := srv.BundlePaths(app.ID, app.ActiveBundleID)

    prevManifest := filepath.Join(bundlePaths.Base, "store-manifest.json.prev")
    if !fileExists(prevManifest) {
        return fmt.Errorf("no previous store-manifest to roll back to")
    }

    currentManifest := filepath.Join(bundlePaths.Base, "store-manifest.json")

    // Swap current ↔ previous.
    tmpPath := currentManifest + ".tmp"
    if err := copyFile(currentManifest, tmpPath); err != nil {
        return err
    }
    if err := copyFile(prevManifest, currentManifest); err != nil {
        return err
    }
    if err := os.Rename(tmpPath, prevManifest); err != nil {
        return err
    }

    // Reassemble workers with the rolled-back store-manifest (graceful drain).
    taskID := srv.Tasks.Create(app.ID, "rollback-refresh")
    sender := srv.Tasks.Sender(taskID)
    sender.Write("rolling back dependencies...")
    srv.drainAndReplace(ctx, app, currentManifest, sender)
    srv.Tasks.Complete(taskID)

    return nil
}
```

The store still holds the old package versions (append-only), so
rollback is instant — no rebuilding, just library reassembly from
the prior store-manifest via hardlinks.

### Rollback API

```go
// internal/api/refresh.go

func PostRefreshRollback(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        appID := chi.URLParam(r, "id")
        caller := auth.CallerFromContext(r.Context())

        app, err := resolveAppRelation(srv, w, caller, appID)
        if err != nil {
            return
        }

        if err := srv.RollbackRefresh(r.Context(), app); err != nil {
            writeJSON(w, http.StatusConflict,
                map[string]string{"message": err.Error()})
            return
        }

        writeJSON(w, http.StatusOK,
            map[string]string{"message": "rollback complete"})
    }
}

// Route:
// r.Post("/api/v1/apps/{id}/refresh/rollback", api.PostRefreshRollback(srv))
```

---

## Step 12: Database changes

### Schema changes

```sql
-- Migration: add refresh columns to apps table.

ALTER TABLE apps ADD COLUMN refresh_schedule TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN refresh_on_start BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE apps ADD COLUMN last_refresh_at TIMESTAMP;
```

### DB methods

```go
// internal/db/db.go

func (d *DB) ListAppsWithRefreshSchedule() ([]App, error) {
    return d.listApps("refresh_schedule != ''")
}

func (d *DB) UpdateLastRefresh(appID string, t time.Time) error {
    _, err := d.db.Exec(
        "UPDATE apps SET last_refresh_at = ? WHERE id = ?", t, appID)
    return err
}
```

---

## Step 13: Tests

### Unit tests

**Worker auth:**
- `TestWorkerTokenGenerate` — token encodes worker ID and app ID.
- `TestWorkerTokenValidate` — valid token passes middleware.
- `TestWorkerTokenInvalid` — invalid/expired/non-worker token rejected.
- `TestWorkerTokenWrongPrefix` — user session token rejected.

**Version conflict detection:**
- `TestDetectConflict_NoConflict` — new package, not loaded.
- `TestDetectConflict_SameCompoundRef` — loaded package's compound ref
  (sourceHash/configHash) matches store-manifest ref → no conflict.
- `TestDetectConflict_DifferentSourceHash` — source hash differs
  (version bump) → conflict.
- `TestDetectConflict_SameSourceDifferentConfig` — same source hash but
  different config hash (LinkingTo ABI change, e.g., sf compiled against
  new Rcpp) → conflict.
- `TestDetectConflict_BasePackage` — base R packages (not in
  `.packages.json`) skipped.
- `TestDetectConflict_NotInNewManifest` — loaded package absent from
  new store-manifest → no conflict.

**Store-manifest comparison:**
- `TestStoreManifestsChanged_Identical` — same packages/refs → false.
- `TestStoreManifestsChanged_VersionBump` — one package ref changed → true.
- `TestStoreManifestsChanged_PackageAdded` — new package in manifest → true.
- `TestStoreManifestsChanged_PackageRemoved` — package removed → true.

**Staging directory:**
- `TestCreateStagingDir` — creates directory under store root.
- `TestCleanupStagingDir` — removes directory.

**Refresh rollback:**
- `TestRollbackRefresh_SwapsStoreManifests` — current ↔ prev swapped.
- `TestRollbackRefresh_NoPrevious` — error when no prev store-manifest.

### Integration tests

- **Runtime install (store hit):** build app with package X → request
  package X from worker → verify instant hardlink, "ok" response.
- **Runtime install (store miss):** request package not in store →
  verify build container runs, package installed and ingested → verify
  hardlink into /lib, "ok" response.
- **Runtime install (version conflict):** request package that would
  change a loaded dependency's version → verify "transfer" response
  with transfer path.
- **Container transfer:** trigger version conflict → write board.json
  to transfer path → verify new worker spawned → verify traffic
  rerouted → verify old worker evicted.
- **Dependency refresh (unchanged):** refresh with no upstream changes
  → verify "no action needed" message, no new worker spawned.
- **Dependency refresh (changed):** change upstream (e.g., advance
  Remotes commit) → refresh → verify new store-manifest, new worker spawned,
  old worker marked draining.
- **Refresh with active sessions:** refresh while sessions are active
  on old worker → verify sessions on old worker continue undisturbed,
  new sessions routed to new worker, old worker evicted only after all
  sessions disconnect.
- **Refresh rollback:** refresh → rollback → verify previous store-manifest
  restored, new worker spawned, old workers drained.
- **Refresh pinned app:** attempt refresh on pinned app → verify 409
  error with clear message.
- **Worker auth:** request packages without token → 401. Request with
  user session token → 403. Request with valid worker token → 200.
- **Scheduled refresh:** set refresh_schedule → advance time → verify
  refresh triggered.

### E2E tests

- Deploy unpinned app → install runtime package → verify app can
  load the new package.
- Deploy unpinned app with Remotes → refresh → verify Remotes
  updated → verify worker library updated → app runs correctly.
- Deploy pinned app → attempt refresh → verify rejection with clear
  error message.

---

## Design Decisions

1. **Same four-phase flow for runtime installs and builds.** Runtime
   package installation uses the exact same store-aware pipeline
   (lockfile → store check → install misses → ingest). The only
   differences are: (a) the worker's `/lib` is a reference library so
   pak sees what's already installed, and (b) the staging directory
   replaces the build library. This eliminates a separate code path
   and ensures runtime installs populate the store the same way builds
   do — a runtime install of package X benefits the next build that
   needs X.

2. **Blocking API with three explicit outcomes.** The packages
   endpoint blocks until the package is available, returning one of
   three statuses: "ok" (installed), "transfer" (version conflict), or
   "error". The R session waits on the HTTP response — no polling, no
   callbacks, no async complexity. The "transfer" status is the signal
   for the R code to serialize board state; the server handles
   everything else. This keeps the R-side code trivial and all
   orchestration on the Go server.

3. **Transfer signaling via atomic rename.** The R session writes
   `board.json.tmp` then `rename()`s to `board.json`. The server polls
   via `stat()` at ~100ms intervals. When the file appears, the write
   is guaranteed complete. This is simpler than HTTP callbacks (no
   endpoint to call back to), inotify (platform dependency), or
   sentinel files (extra coordination). The poll has a timeout — if
   the file doesn't appear, the server aborts.

4. **Worker token as long-lived HMAC.** The worker token is generated
   at spawn time with a long TTL. The token is effectively revoked
   when the worker is evicted (the worker ID is no longer recognized).
   Using the existing HMAC-SHA256 signing infrastructure (from session
   tokens) avoids adding a new auth mechanism. The `worker:` prefix
   in the Sub claim prevents worker tokens from being used as user
   session tokens.

5. **Refresh re-runs the full build pipeline with graceful drain.**
   Rather than a lightweight "re-resolve and diff" flow, refresh runs
   the complete store-aware build pipeline (lockfile → store check →
   install misses → ingest). This reuses the existing build code (no
   new pipeline) and ensures any new dependency versions are properly
   ingested into the store. The store-manifest comparison after the
   build detects whether anything actually changed — if not, no new worker
   is spawned. When dependencies have changed, a new worker is spawned
   with the updated library and old workers are drained gracefully
   rather than swapped immediately.

6. **Single-depth rollback (prev store-manifest).** Refresh retains
   exactly one previous store-manifest. Multiple levels of rollback
   would require a manifest history table — complexity that can be added later if
   needed. The store is append-only, so even old package versions
   remain available indefinitely (until eviction). For deeper
   rollback, redeploy from the original source.

7. **Refresh as a server-side operation, not a rebuild.** Refresh does
   not re-upload code or create a new bundle. It re-resolves
   dependencies using the original unpinned manifest (preserved from
   the initial deploy) and the same repository URLs. The bundle's
   code is unchanged — only the dependency versions move forward.
   This makes refresh fast (no upload, no scan) and safe (code is
   the same, only deps change).

8. **Scheduled refresh via lightweight polling, not cron library.**
   The scheduler checks once per minute whether any app's cron
   expression fires. This avoids adding a cron library dependency for
   what is fundamentally a simple timer. The per-minute poll is
   sufficient because refresh operations take seconds to minutes —
   sub-minute scheduling precision is not needed.

9. **Graceful drain on refresh, transfer only on live conflicts.**
   Refresh spawns a new worker and drains old ones — running sessions
   continue undisturbed on the old worker until they disconnect, and
   stale workers are evicted when they have zero active sessions. This
   avoids the disruption and complexity of serializing board state
   during a background operation the user did not initiate. Board
   serialization and container transfer are reserved exclusively for
   live install conflicts (step 8), where the user's action triggered
   the conflict and the session must migrate to continue.
