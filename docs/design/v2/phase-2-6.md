# Phase 2-6: Package Store and Runtime Assembly

A server-level content-addressable package store that is populated
during builds and consumed at runtime. Every dependency restore adds
packages to the store. Workers assemble their libraries from store
entries via hard links — near-instant, zero additional disk usage.
An API endpoint allows requesting packages from the store for running
workers (e.g., during blockr board restore).

Depends on phase 2-5 (pak-based build pipeline).

## Deliverables

1. **Package store** (`internal/pkgstore/store.go`) — content-
   addressable directory keyed by `{package}/{version}-{source}`.
   Populated during builds. Append-only.
2. **Build integration** — after a successful dependency restore,
   catalog all installed packages into the store.
3. **Per-worker library views** (`internal/pkgstore/view.go`) — flat
   directories populated with hard links from the store. Mounted
   read-only at `/extra-lib/` in worker containers.
4. **Worker lifecycle integration** — create view directories on spawn,
   mount them, clean up on eviction.
5. **Runtime assembly API** — `POST /api/v1/packages` endpoint that
   hard-links packages from the store into a running worker's view.
6. **Worker authentication** — HMAC-based worker tokens for
   in-container API access.

---

## Step 1: Package store

New package: `internal/pkgstore/`.

### Directory layout

```
{bundle_server_path}/.pkg-store/
├── ggplot2/
│   └── 3.5.0-cran/           ← installed R package tree
├── shiny/
│   └── 1.13.0-cran/
├── blockr.ggplot/
│   ├── 0.2.0-cran/
│   └── 0.2.1-github/
└── ...
```

Keyed by `{package}/{version}-{source}`. Multiple versions and sources
coexist. Append-only — packages are never modified after installation.

The R major.minor version is NOT part of the key. The store lives under
a server that runs a single R version (configured via `[docker] image`).
When the image changes, the operator clears the store
(`rm -rf .pkg-store/`) — same as clearing any package cache after an
R upgrade. This keeps store keys simple and avoids a startup-time R
version detection container.

### Store struct

```go
// internal/pkgstore/store.go

type Store struct {
    basePath string // {bundle_server_path}/.pkg-store
}

func NewStore(basePath string) *Store {
    return &Store{basePath: basePath}
}

func storeKey(pkg, version, source string) string {
    return filepath.Join(pkg, version+"-"+source)
}

func (s *Store) Path(pkg, version, source string) string {
    return filepath.Join(s.basePath, storeKey(pkg, version, source))
}

func (s *Store) Has(pkg, version, source string) bool {
    _, err := os.Stat(s.Path(pkg, version, source))
    return err == nil
}
```

### DESCRIPTION parser

```go
// internal/pkgstore/description.go

type PkgInfo struct {
    Name       string
    Version    string
    Source     string // "cran", "github", etc.; derived from RemoteType or defaulted
}

func ReadPkgInfo(pkgDir string) (PkgInfo, error) {
    data, err := os.ReadFile(filepath.Join(pkgDir, "DESCRIPTION"))
    if err != nil {
        return PkgInfo{}, err
    }
    var info PkgInfo
    for _, line := range strings.Split(string(data), "\n") {
        switch {
        case strings.HasPrefix(line, "Package:"):
            info.Name = strings.TrimSpace(line[len("Package:"):])
        case strings.HasPrefix(line, "Version:"):
            info.Version = strings.TrimSpace(line[len("Version:"):])
        case strings.HasPrefix(line, "RemoteType:"):
            info.Source = strings.TrimSpace(line[len("RemoteType:"):])
        }
    }
    if info.Name == "" || info.Version == "" {
        return PkgInfo{}, fmt.Errorf("invalid DESCRIPTION in %s", pkgDir)
    }
    if info.Source == "" {
        info.Source = "cran"
    }
    return info, nil
}
```

### Ingest — add packages to the store

```go
// Ingest catalogs all R packages in a library directory and moves
// new ones into the store. Returns the list of all packages found.
// Existing store entries are skipped (not overwritten).
func (s *Store) Ingest(libraryPath string) ([]PkgInfo, error) {
    entries, err := os.ReadDir(libraryPath)
    if err != nil {
        return nil, fmt.Errorf("read library: %w", err)
    }

    var pkgs []PkgInfo
    for _, entry := range entries {
        if !entry.IsDir() {
            continue
        }
        info, err := ReadPkgInfo(filepath.Join(libraryPath, entry.Name()))
        if err != nil {
            slog.Debug("pkgstore: skipping non-package dir",
                "name", entry.Name(), "error", err)
            continue
        }

        storePath := s.Path(info.Name, info.Version, info.Source)
        if !dirExists(storePath) {
            if err := os.MkdirAll(filepath.Dir(storePath), 0o755); err != nil {
                return nil, fmt.Errorf("create store dir: %w", err)
            }
            // cp -al: hard-link the package tree into the store.
            // The source (library) is kept intact for the current bundle.
            out, cpErr := exec.Command(
                "cp", "-al",
                filepath.Join(libraryPath, entry.Name()),
                storePath,
            ).CombinedOutput()
            if cpErr != nil {
                slog.Warn("pkgstore: ingest failed",
                    "package", info.Name, "error", string(out))
                continue
            }
        }

        pkgs = append(pkgs, info)
    }
    return pkgs, nil
}
```

`cp -al` hard-links the package directory tree into the store. Both
the original library (used by the current bundle) and the store entry
point to the same inodes. Zero additional disk usage.

---

## Step 2: Build integration

After a successful dependency restore, ingest the restored library
into the store.

In `internal/bundle/restore.go`, after `ActivateBundle`:

```go
// Populate package store.
if p.PkgStore != nil {
    pkgs, err := p.PkgStore.Ingest(p.Paths.Library)
    if err != nil {
        slog.Warn("pkgstore: ingest failed", "error", err)
        // Non-fatal — the bundle is still usable.
    } else {
        slog.Info("pkgstore: ingested packages",
            "count", len(pkgs), "bundle_id", p.BundleID)
    }
}
```

**RestoreParams** gains a `PkgStore *pkgstore.Store` field.

Every deploy populates the store. Over time, the store accumulates
packages from all apps. When a worker needs a package that any prior
build installed, it's a store hit — hard-link, instant.

---

## Step 3: Per-worker library views

```go
// internal/pkgstore/view.go

type View struct {
    basePath string // {bundle_server_path}/.worker-libs
}

func NewView(basePath string) *View {
    return &View{basePath: basePath}
}

func (v *View) BasePath() string { return v.basePath }

func (v *View) Dir(workerID string) string {
    return filepath.Join(v.basePath, workerID)
}

func (v *View) Create(workerID string) error {
    return os.MkdirAll(v.Dir(workerID), 0o755)
}

// Link hard-links a package from the store into a worker's view.
func (v *View) Link(workerID, pkgName, storePath string) error {
    viewPkgPath := filepath.Join(v.Dir(workerID), pkgName)
    if dirExists(viewPkgPath) {
        return nil // already linked
    }
    out, err := exec.Command("cp", "-al", storePath, viewPkgPath).
        CombinedOutput()
    if err != nil {
        return fmt.Errorf("hard-link %s: %s: %w", pkgName, out, err)
    }
    return nil
}

func (v *View) Cleanup(workerID string) error {
    dir := v.Dir(workerID)
    if _, err := os.Stat(dir); os.IsNotExist(err) {
        return nil
    }
    return os.RemoveAll(dir)
}
```

**Same-filesystem constraint:** hard links require source and target
on the same filesystem. Both store and views live under
`bundle_server_path`, naturally satisfied in all mount modes.

**Mount propagation:** when packages are linked into a running
worker's view on the host side, they appear inside the container
immediately — bind mounts share the underlying filesystem. The `ro`
flag only prevents writes from inside the container.

---

## Step 4: Worker lifecycle integration

### WorkerSpec changes

```go
// internal/backend/backend.go

type WorkerSpec struct {
    // ... existing fields ...
    ExtraLibPath string // server-side path to extra-lib view dir; empty if unused
}
```

### WorkerMounts

Extend `WorkerMounts` to include the extra-lib mount when set:

```go
func (mc MountConfig) WorkerMounts(
    bundlePath, libraryPath, workerMount, extraLibPath string,
) (binds []string, mounts []mount.Mount) {
    // ... existing bundle + library mount logic ...

    if extraLibPath != "" {
        // same translation logic as other mounts (Volume/Bind/Native)
    }
    return binds, mounts
}
```

### R_LIBS

Update `R_LIBS` in `createWorkerContainer` to search `/extra-lib`
first:

```go
"R_LIBS=/extra-lib:/blockyard-lib"
```

When `/extra-lib` is empty, R silently ignores it. Live-installed
packages shadow the base library because `/extra-lib` comes first.

### Spawn integration

In `spawnWorker()`, create the view directory and pass the path:

```go
var extraLibPath string
if srv.PkgView != nil {
    if err := srv.PkgView.Create(wid); err != nil {
        return "", "", fmt.Errorf("create pkg view: %w", err)
    }
    extraLibPath = srv.PkgView.Dir(wid)
}

spec := backend.WorkerSpec{
    // ... existing fields ...
    ExtraLibPath: extraLibPath,
}
```

### Eviction cleanup

In `EvictWorker()`, clean up the view after the container stops:

```go
if srv.PkgView != nil {
    if err := srv.PkgView.Cleanup(workerID); err != nil {
        slog.Warn("evict: failed to clean pkg view",
            "worker_id", workerID, "error", err)
    }
}
```

### Server struct

```go
type Server struct {
    // ... existing fields ...
    PkgStore *pkgstore.Store // nil when not available
    PkgView  *pkgstore.View // nil when not available
}
```

### Startup cleanup

Remove orphaned view directories from previous runs:

```go
if srv.PkgView != nil {
    entries, _ := os.ReadDir(srv.PkgView.BasePath())
    for _, e := range entries {
        if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
            continue
        }
        if _, found := srv.Workers.Get(e.Name()); !found {
            os.RemoveAll(filepath.Join(srv.PkgView.BasePath(), e.Name()))
        }
    }
}
```

---

## Step 5: Runtime assembly API

### Endpoint

```
POST /api/v1/packages
{
    "worker_id": "...",
    "packages": [
        { "name": "ggplot2", "version": "3.5.0", "source": "cran" },
        { "name": "blockr.ggplot" }
    ]
}
→ 200 { "installed": [...], "missing": [...] }
→ 400 invalid request
→ 404 worker not found
→ 501 package store not enabled
```

The endpoint looks up each requested package in the store. Packages
found in the store are hard-linked into the worker's view (instant).
Packages not found are returned in a `missing` list — the caller
decides what to do (deploy an app that includes them, or accept the
gap).

```go
// internal/api/packages.go

func InstallPackages(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if srv.PkgStore == nil {
            http.Error(w, "package store not enabled",
                http.StatusNotImplemented)
            return
        }

        var body installPackagesRequest
        if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
            badRequest(w, "invalid JSON body")
            return
        }
        // ... validate worker_id, packages, auth ...

        worker, found := srv.Workers.Get(body.WorkerID)
        if !found {
            notFound(w, "worker not found")
            return
        }

        var installed, missing []pkgResult
        for _, pkg := range body.Packages {
            source := pkg.Source
            if source == "" {
                source = "cran"
            }

            if pkg.Version != "" && srv.PkgStore.Has(pkg.Name, pkg.Version, source) {
                storePath := srv.PkgStore.Path(pkg.Name, pkg.Version, source)
                if err := srv.PkgView.Link(
                    body.WorkerID, pkg.Name, storePath,
                ); err != nil {
                    serverError(w, fmt.Sprintf("link %s: %s", pkg.Name, err))
                    return
                }
                installed = append(installed, pkgResult{
                    Name: pkg.Name, Version: pkg.Version, Source: source,
                })
            } else {
                // Version not specified or not in store — try to find
                // any version of this package.
                if found := srv.PkgStore.FindLatest(pkg.Name, source); found != nil {
                    if err := srv.PkgView.Link(
                        body.WorkerID, pkg.Name, found.Path,
                    ); err != nil {
                        serverError(w, fmt.Sprintf("link %s: %s", pkg.Name, err))
                        return
                    }
                    installed = append(installed, pkgResult{
                        Name: found.Name, Version: found.Version, Source: found.Source,
                    })
                } else {
                    missing = append(missing, pkgResult{
                        Name: pkg.Name, Version: pkg.Version, Source: source,
                    })
                }
            }
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(installPackagesResponse{
            Installed: installed,
            Missing:   missing,
        })
    }
}
```

### FindLatest

```go
// FindLatest returns the newest version of a package in the store,
// or nil if not found. Uses directory modification time as a proxy
// for recency.
func (s *Store) FindLatest(pkg, source string) *PkgInfo {
    pkgDir := filepath.Join(s.basePath, pkg)
    entries, err := os.ReadDir(pkgDir)
    if err != nil {
        return nil
    }
    suffix := "-" + source
    var best *PkgInfo
    var bestTime time.Time
    for _, e := range entries {
        if !e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
            continue
        }
        info, err := e.Info()
        if err != nil {
            continue
        }
        if best == nil || info.ModTime().After(bestTime) {
            version := strings.TrimSuffix(e.Name(), suffix)
            best = &PkgInfo{Name: pkg, Version: version, Source: source}
            best.Path = filepath.Join(pkgDir, e.Name())
            bestTime = info.ModTime()
        }
    }
    return best
}
```

### Route registration

```go
r.Post("/packages", InstallPackages(srv))
```

---

## Step 6: Worker authentication

The R app inside a worker needs to call the package assembly API.
HMAC-based worker tokens provide lightweight, stateless auth for
in-container callers.

```go
// internal/auth/workertoken.go

func WorkerToken(sessionSecret []byte, workerID string) string {
    mac := hmac.New(sha256.New, sessionSecret)
    mac.Write([]byte("blockyard-worker:" + workerID))
    return hex.EncodeToString(mac.Sum(nil))
}

func ValidateWorkerToken(sessionSecret []byte, workerID, token string) bool {
    expected := WorkerToken(sessionSecret, workerID)
    return hmac.Equal([]byte(expected), []byte(token))
}
```

Injected at spawn time as `BLOCKYARD_WORKER_TOKEN` and
`BLOCKYARD_WORKER_ID` environment variables. The API accepts either
standard auth (session cookie / PAT) or
`Authorization: Worker <id>:<token>`.

R side:

```r
worker_id <- Sys.getenv("BLOCKYARD_WORKER_ID")
token     <- Sys.getenv("BLOCKYARD_WORKER_TOKEN")
api_url   <- Sys.getenv("BLOCKYARD_API_URL")

resp <- httr::POST(
    paste0(api_url, "/api/v1/packages"),
    body = list(worker_id = worker_id,
                packages = list(list(name = "ggplot2"))),
    encode = "json",
    httr::add_headers(
        Authorization = paste0("Worker ", worker_id, ":", token)))

# resp$missing tells the app which packages aren't available.
```

---

## Step 7: Tests

### Unit tests

**Package store:**
- `TestStoreKey` — format: `{name}/{version}-{source}`.
- `TestStoreHas` — empty store → false; create entry → true.
- `TestIngest` — create temp library with mock packages, ingest,
  verify store entries created with correct hard links.
- `TestIngestIdempotent` — ingest same library twice, second is no-op.
- `TestFindLatest` — multiple versions, returns most recent.
- `TestReadPkgInfo` — valid and invalid DESCRIPTION files.

**Views:**
- `TestViewCreate`, `TestViewLink`, `TestViewCleanup`,
  `TestViewLinkIdempotent` — same as before.

**Worker token:**
- `TestWorkerToken`, `TestWorkerTokenWrongSecret`,
  `TestWorkerTokenWrongWorkerID`.

### Integration tests

- Deploy app → verify packages ingested into store.
- Deploy second app with overlapping deps → verify no duplicate store
  entries (hard links to same inodes).
- POST /api/v1/packages for a package in the store → 200, installed.
- POST for a package not in the store → 200, missing list populated.
- POST for non-existent worker → 404.
- Worker token auth works; wrong token → rejected.

---

## Design Decisions

1. **Builds populate the store; runtime consumes it.** The store is
   filled during dependency restore — a process that already takes
   seconds to minutes. Adding store ingestion is near-free (hard links
   take milliseconds). Runtime assembly is always instant (hard-link
   from store to view). No build containers at runtime.

2. **Store misses return `missing`, not errors.** The API reports which
   packages weren't found rather than failing. The caller (R app)
   decides how to handle gaps — degrade gracefully, show a message, or
   request a redeploy. This keeps the runtime path non-blocking.

3. **No R version in store key.** The server runs a single R version.
   On R upgrades, clear the store. This avoids startup-time R version
   detection and keeps keys simple.

4. **`cp -al` for both ingest and view linking.** Hard links give zero
   disk overhead and instant creation. The same-filesystem constraint
   is naturally satisfied (everything under `bundle_server_path`).

5. **No package cleanup/GC in v2.** Append-only. At ~10–50 MB per
   package, hundreds of packages use only a few GB. Store cleanup
   (LRU eviction, size limits) is a v3 concern.

6. **`R_LIBS` always includes `/extra-lib`.** Even without the package
   store enabled, R silently ignores non-existent paths. No
   conditional logic needed.

## Open Questions

1. **Store miss at runtime — future options.** v2 returns missing
   packages to the caller. Future phases could add: (a) worker-side
   `install.packages()` into a rw extra-lib mount for immediate
   fallback, (b) async background build that populates the store, or
   (c) a "pre-populate store" admin API. The right answer depends on
   real-world usage patterns.
