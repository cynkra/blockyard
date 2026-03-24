# Phase 2-6: Package Store & Worker Library Assembly

A server-level content-addressable package store populated during builds
and consumed at worker startup. The store caches installed R packages
keyed by a curated hash of identity fields from the pak lockfile. Build
libraries are pre-populated from the store; workers assemble their
libraries via hard links. This phase retrofits the phase 2-5 build flow
to the store-aware four-phase pattern described in dep-mgmt.md.

Depends on phase 2-5 (manifest types, pak build pipeline, lockfile
output).

See [dep-mgmt.md](../dep-mgmt.md) for store key design, ABI safety
rationale, concurrency protocol, store layout, and design rationale.
This document covers how to build the store and integrate it with the
build pipeline and worker lifecycle.

## Deliverables

1. **Package store** (`internal/pkgstore/store.go`) — content-addressable
   directory keyed by `{platform}/{package}/{curated_hash}`. The
   `platform` prefix encodes R version (minor), OS, and architecture
   (e.g., `4.4-linux-x86_64`). Curated hash is SHA-256 of selected
   identity fields from the pak lockfile entry.
2. **Store operations** — `Has(key)`, `Path(key)`, `Ingest(key, src)`.
   Ingestion uses atomic `rename()` from a build directory on the same
   filesystem. Append-only — packages are never modified after insertion.
3. **Store concurrency** — file-based locking under
   `{root}/.locks/{platform}/{package}/{hash}.lock`. Concurrent builds
   wait with backoff for the lock holder to finish. Stale lock detection
   via age threshold.
4. **Store-aware build flow** — retrofit the phase 2-5 build to the
   four-phase pattern: (1) `lockfile_create()` → (2) check store,
   pre-populate build library with hard links → (3) `lockfile_install()`
   skips pre-populated packages → (4) ingest newly installed packages
   into store. Full store hits make phase 3 a no-op.
5. **Persistent pak download cache** — already set up in phase 2-5;
   this phase ensures the mount is present in the store-aware build flow.
6. **Worker library assembly** — at worker startup, assemble a single
   mutable `/lib` per container by hard-linking from the store based on
   the bundle's pak lockfile. Each lockfile entry maps to a store path
   via the curated hash. Assembly also writes a `.packages.json`
   manifest (`{package: store_key}`) that tracks what's installed in
   each worker — the source of truth for live installs (phase 2-7).
7. **Worker lifecycle integration** — create `/lib` directories on spawn,
   populate from store, mount into containers. Clean up on eviction.
8. **`by-builder` binary** (`cmd/by-builder/`) — a small Go CLI
   cross-compiled for `linux/amd64` and `linux/arm64`, cached on the
   server (same pattern as pakcache), and mounted read-only into build
   containers at `/tools/by-builder`. Provides `store populate` and
   `store ingest` subcommands that handle all store operations inside
   the container. Shares `internal/pkgstore` with the server — store
   key computation, locking, ABI checks, and metadata exist in one
   place. No R-side store code needed.

---

## Step 1: Package store types

New package: `internal/pkgstore/`.

### Store struct

```go
// internal/pkgstore/store.go

type Store struct {
    root     string // host-side store root, e.g., {bundle_server_path}/.pkg-store
    platform string // e.g., 4.4-linux-x86_64; set via DetectPlatform or SetPlatform
}

func NewStore(root string) *Store {
    return &Store{root: root}
}

func (s *Store) Root() string     { return s.root }
func (s *Store) Platform() string { return s.platform }

func (s *Store) SetPlatform(p string) { s.platform = p }
```

### Store layout

```
{root}/
├── 4.4-linux-x86_64/            ← platform prefix
│   ├── shiny/
│   │   ├── e3b0c442.../         ← installed package tree
│   │   ├── e3b0c442....json     ← store metadata (sibling file)
│   │   ├── 7d865e95.../         ← different version/hash
│   │   └── 7d865e95....json
│   ├── ggplot2/
│   │   ├── b5bb9d80.../
│   │   └── b5bb9d80....json
│   └── ...
├── .builds/                      ← temporary build libraries
│   └── {uuid}/
├── .workers/                     ← per-worker assembled libraries
│   └── {worker_id}/
│       ├── .packages.json       ← per-worker package manifest
│       ├── shiny/               ← hard-linked package trees
│       └── ...
└── .locks/                       ← concurrency locks
    └── 4.4-linux-x86_64/
        └── shiny/
            └── e3b0c442.lock
```

Each `{hash}/` directory contains the installed package tree directly
(`DESCRIPTION`, `R/`, `Meta/`, etc. — no nested package name
directory). The sibling `{hash}.json` file holds store metadata.

All helper directories (`.builds/`, `.workers/`, `.locks/`) live under
the store root to guarantee same-filesystem placement for hard links
and atomic `rename()`.

### LockfileEntry

Parsed from the pak lockfile (JSON). The curated hash and worker
library assembly are computed from these fields.

```go
// internal/pkgstore/lockfile.go

type LockfileEntry struct {
    Package      string `json:"package"`
    Version      string `json:"version"`
    RemoteType   string `json:"RemoteType"`
    SHA256       string `json:"sha256"`
    RemoteSha    string `json:"RemoteSha"`
    RemoteSubdir string `json:"RemoteSubdir"`
}

type Lockfile struct {
    RVersion string          `json:"r_version"`
    OS       string          `json:"os"`
    Arch     string          `json:"arch"`
    Packages []LockfileEntry `json:"packages"`
}

func ReadLockfile(path string) (*Lockfile, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err
    }
    var lf Lockfile
    if err := json.Unmarshal(data, &lf); err != nil {
        return nil, fmt.Errorf("parse pak lockfile: %w", err)
    }
    return &lf, nil
}
```

### Store key computation

The curated hash dispatches on `RemoteType` and hashes only the fields
that determine what source code was compiled. See dep-mgmt.md
§ Package Store and Cache Key Design for the full rationale.

```go
// internal/pkgstore/key.go

// StoreKey computes the curated hash for a pak lockfile entry.
// The hash is SHA-256 of a NUL-delimited string of identity fields.
//
// This is the single implementation — the by-builder binary and the
// server both use this function. No R-side equivalent exists.
//
// Hash input format:
//     {RemoteType}\0{field1}\0{field2}[...\0{fieldN}]
func StoreKey(entry LockfileEntry) (string, error) {
    var fields []string
    switch entry.RemoteType {
    case "standard":
        fields = []string{entry.Package, entry.Version, entry.SHA256}
    case "github", "gitlab", "git":
        fields = []string{entry.Package, entry.RemoteSha, entry.RemoteSubdir}
    default:
        return "", fmt.Errorf(
            "unsupported RemoteType for store key: %q", entry.RemoteType)
    }

    input := entry.RemoteType + "\x00" + strings.Join(fields, "\x00")
    h := sha256.Sum256([]byte(input))
    return hex.EncodeToString(h[:]), nil
}
```

| RemoteType | Identity fields | Rationale |
|---|---|---|
| `standard` | `package`, `version`, `sha256` | `sha256` (archive hash) catches PPM rebuilds where the version is unchanged but the binary differs. |
| `github`, `gitlab`, `git` | `package`, `RemoteSha`, `RemoteSubdir` | The commit hash fully identifies the source code. `RemoteSubdir` selects which package within a monorepo. |

Unsupported: `url::` (lacks reliable content hash) and `local::`
(would need source tree hashing). Both are niche; `StoreKey()` returns
an error. See dep-mgmt.md § Unsupported ref types.

### Store operations

```go
// Path returns the store path for a package entry.
func (s *Store) Path(pkg, hash string) string {
    return filepath.Join(s.root, s.platform, pkg, hash)
}

// MetaPath returns the metadata file path for a store entry.
func (s *Store) MetaPath(pkg, hash string) string {
    return filepath.Join(s.root, s.platform, pkg, hash+".json")
}

// Has reports whether the store contains a package with the given key.
func (s *Store) Has(pkg, hash string) bool {
    _, err := os.Stat(s.Path(pkg, hash))
    return err == nil
}

// Ingest atomically moves an installed package tree into the store.
// No-op if the entry already exists. srcDir must be on the same
// filesystem as the store (for atomic rename).
func (s *Store) Ingest(pkg, hash, srcDir string) error {
    dst := s.Path(pkg, hash)
    if dirExists(dst) {
        return nil // already in store
    }
    if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
        return fmt.Errorf("create store dir: %w", err)
    }
    return os.Rename(srcDir, dst)
}

// Touch updates the mtime of a store entry's metadata file.
// Used for last-accessed tracking (LRU eviction, future concern).
func (s *Store) Touch(pkg, hash string) {
    metaPath := s.MetaPath(pkg, hash)
    now := time.Now()
    os.Chtimes(metaPath, now, now)
}
```

### Store metadata

Each store entry has a sibling JSON file containing metadata for ABI
safety checks and eviction. See dep-mgmt.md § Store Layout.

```go
// internal/pkgstore/meta.go

type StoreMeta struct {
    CreatedAt      time.Time         `json:"created_at"`
    SourceCompiled bool              `json:"source_compiled"`
    LinkingTo      map[string]string `json:"linkingto,omitempty"` // pkg name → store key
}

func WriteStoreMeta(path string, meta StoreMeta) error {
    data, err := json.MarshalIndent(meta, "", "  ")
    if err != nil {
        return err
    }
    return os.WriteFile(path, data, 0o644)
}

func ReadStoreMeta(path string) (StoreMeta, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return StoreMeta{}, err
    }
    var meta StoreMeta
    return meta, json.Unmarshal(data, &meta)
}
```

| Field | Description |
|---|---|
| `created_at` | Timestamp written once at ingestion. |
| `source_compiled` | Whether this entry was compiled from source (vs pre-built binary). |
| `linkingto` | Map of `LinkingTo` dependency names to their store keys at compile time. Empty for binary installs or packages without `LinkingTo`. |

**Last-accessed tracking** uses the metadata file's filesystem `mtime`
rather than a JSON field — the server `touch`es the metadata file on
every store hit. `touch` is a metadata-only syscall (`utimes()`),
avoiding JSON rewrites on every access.

---

## Step 2: Platform detection

The store key includes a platform prefix (`{R_major}.{R_minor}-linux-{arch}`)
to prevent mixing packages compiled under different R versions or
architectures. The platform is derived from the pak lockfile — both the
server (worker assembly) and the `by-builder` binary (store operations
inside build containers) use `PlatformFromLockfile()`.

Derived from the pak lockfile after the first successful build, then
cached in the `Store` struct.

```go
// internal/pkgstore/platform.go

// PlatformFromLockfile derives the platform prefix from a pak lockfile.
// R version is truncated to major.minor (e.g., "4.4.2" → "4.4").
func PlatformFromLockfile(lf *Lockfile) string {
    parts := strings.SplitN(lf.RVersion, ".", 3)
    rVersion := parts[0] + "." + parts[1]
    return rVersion + "-" + lf.OS + "-" + lf.Arch
}
```

The platform is set after the first build:

```go
if s.platform == "" {
    lf, err := ReadLockfile(lockfilePath)
    if err == nil {
        s.SetPlatform(PlatformFromLockfile(lf))
    }
}
```

On subsequent server restarts, the platform is recovered by scanning
for existing lockfiles or existing platform directories in the store
root.

**Format:** `{major}.{minor}-linux-{arch}`, e.g., `4.4-linux-x86_64`.

---

## Step 3: Store concurrency

The store is shared across all builds running on the same server. When
concurrent builds need the same package, the first build to start
installing it takes a lock; subsequent builds wait for the lock to
release and then use the store entry.

### Lock protocol

1. Before ingesting a package, the build acquires a lock at
   `{root}/.locks/{platform}/{package}/{hash}.lock`. The lock is a
   directory (created atomically via `mkdir`).
2. If the lock directory already exists, the build waits with
   exponential backoff (0.5–2s, jittered) until the directory is
   removed and the store entry appears.
3. After successful ingestion (package tree + metadata file written),
   the build removes the lock directory.
4. **Stale lock detection:** if the lock directory's mtime is older than
   30 minutes, the waiting build removes it and re-attempts acquisition.
   This handles crashed builds that never released their lock.

Using `mkdir` (not file creation) because directory creation is atomic
on all POSIX filesystems — exactly one concurrent caller succeeds.

### Go implementation

Locking is implemented in `internal/pkgstore/lock.go` and runs inside
build containers via the `by-builder store ingest` subcommand. The
same code runs on the server for any host-side store operations.

```go
// internal/pkgstore/lock.go

func (s *Store) LockPath(pkg, hash string) string {
    return filepath.Join(s.root, ".locks", s.platform, pkg, hash+".lock")
}

func (s *Store) Acquire(pkg, hash string, staleThreshold time.Duration) error {
    lockDir := s.LockPath(pkg, hash)
    os.MkdirAll(filepath.Dir(lockDir), 0o755)

    for {
        if err := os.Mkdir(lockDir, 0o755); err == nil {
            return nil // acquired
        }
        // Check for stale lock.
        info, err := os.Stat(lockDir)
        if err == nil && time.Since(info.ModTime()) > staleThreshold {
            os.RemoveAll(lockDir)
            continue
        }
        // Wait with jittered backoff.
        time.Sleep(time.Duration(500+rand.Intn(1500)) * time.Millisecond)
    }
}

func (s *Store) Release(pkg, hash string) {
    os.RemoveAll(s.LockPath(pkg, hash))
}
```

---

## Step 4: by-builder binary

A small Go CLI that runs inside build containers, providing store
operations as subcommands. The binary shares `internal/pkgstore` with
the server — store key computation, locking, ABI checks, and metadata
live in one place.

### Binary structure

```go
// cmd/by-builder/main.go

func main() {
    root := &cobra.Command{Use: "by-builder"}
    store := &cobra.Command{Use: "store"}
    store.AddCommand(populateCmd(), ingestCmd())
    root.AddCommand(store)
    root.Execute()
}
```

### `store populate` subcommand

Reads the pak lockfile, checks the store for each entry, and
hard-links hits into the build library. Packages already present in
the build library (or in an optional reference library) are skipped.

```go
// cmd/by-builder/populate.go

func populateCmd() *cobra.Command {
    var lockfile, lib, storeRoot, refLib string
    cmd := &cobra.Command{
        Use: "populate",
        RunE: func(cmd *cobra.Command, args []string) error {
            lf, err := pkgstore.ReadLockfile(lockfile)
            if err != nil {
                return err
            }
            s := pkgstore.NewStore(storeRoot)
            s.SetPlatform(pkgstore.PlatformFromLockfile(lf))

            var hits, misses int
            for _, entry := range lf.Packages {
                // Skip packages already in the reference library.
                if refLib != "" && dirExists(filepath.Join(refLib, entry.Package)) {
                    continue
                }
                // Skip packages already in the build library.
                if dirExists(filepath.Join(lib, entry.Package)) {
                    continue
                }

                key, err := pkgstore.StoreKey(entry)
                if err != nil {
                    return err
                }
                if !s.Has(entry.Package, key) {
                    misses++
                    continue
                }

                // ABI safety check for source-compiled packages.
                if !s.LinkingToMatches(entry.Package, key, lf) {
                    misses++ // ABI mismatch — treat as store miss
                    continue
                }

                // Hard-link store entry into build library.
                dest := filepath.Join(lib, entry.Package)
                if err := hardlink(s.Path(entry.Package, key), dest); err != nil {
                    return fmt.Errorf("link %s: %w", entry.Package, err)
                }
                s.Touch(entry.Package, key)
                hits++
            }

            fmt.Fprintf(os.Stderr, "store: %d hits, %d misses\n", hits, misses)
            return nil
        },
    }
    cmd.Flags().StringVar(&lockfile, "lockfile", "", "path to pak.lock")
    cmd.Flags().StringVar(&lib, "lib", "", "build library path")
    cmd.Flags().StringVar(&storeRoot, "store", "", "store root path")
    cmd.Flags().StringVar(&refLib, "reference-lib", "", "skip packages present here (optional)")
    return cmd
}
```

### `store ingest` subcommand

Ingests newly installed packages from the build library into the store.
Packages already in the store are skipped. Locking ensures concurrent
builds don't conflict.

```go
// cmd/by-builder/ingest.go

func ingestCmd() *cobra.Command {
    var lockfile, lib, storeRoot, refLib string
    cmd := &cobra.Command{
        Use: "ingest",
        RunE: func(cmd *cobra.Command, args []string) error {
            lf, err := pkgstore.ReadLockfile(lockfile)
            if err != nil {
                return err
            }
            s := pkgstore.NewStore(storeRoot)
            s.SetPlatform(pkgstore.PlatformFromLockfile(lf))

            for _, entry := range lf.Packages {
                // Skip packages from the reference library (not built here).
                if refLib != "" && dirExists(filepath.Join(refLib, entry.Package)) {
                    continue
                }

                key, err := pkgstore.StoreKey(entry)
                if err != nil {
                    return err
                }
                if s.Has(entry.Package, key) {
                    continue // already in store
                }

                pkgPath := filepath.Join(lib, entry.Package)
                if !dirExists(pkgPath) {
                    continue // not installed (shouldn't happen)
                }

                // Ingest under lock.
                s.Acquire(entry.Package, key, 30*time.Minute)
                if !s.Has(entry.Package, key) { // re-check after lock
                    s.Ingest(entry.Package, key, pkgPath)
                    s.WriteIngestMeta(entry, lf)
                }
                s.Release(entry.Package, key)

                fmt.Fprintf(os.Stderr, "store: ingested %s\n", entry.Package)
            }
            return nil
        },
    }
    cmd.Flags().StringVar(&lockfile, "lockfile", "", "path to pak.lock")
    cmd.Flags().StringVar(&lib, "lib", "", "build library path")
    cmd.Flags().StringVar(&storeRoot, "store", "", "store root path")
    cmd.Flags().StringVar(&refLib, "reference-lib", "", "skip packages present here (optional)")
    return cmd
}
```

### Caching

The binary is cross-compiled at release time for `linux/amd64` and
`linux/arm64`. At runtime the server selects the correct binary based
on `runtime.GOARCH` (the container runs on the same host as the server).

```go
// internal/buildercache/buildercache.go

// EnsureCached returns the path to the by-builder binary for the
// current platform. The binary is shipped alongside the server and
// cached at {cachePath}/by-builder-{version}-linux-{arch}.
func EnsureCached(cachePath, version string) (string, error) {
    name := fmt.Sprintf("by-builder-%s-linux-%s", version, runtime.GOARCH)
    binPath := filepath.Join(cachePath, name)
    if fileExists(binPath) {
        return binPath, nil
    }
    // Extract from embedded binary or download from release artifacts.
    // Details depend on the release/distribution strategy.
    return binPath, extract(binPath, name)
}
```

**Cache path:** `{bundle_server_path}/.by-builder-cache/`

The binary is mounted read-only into every build container at
`/tools/by-builder`.

---

## Step 5: Store-aware build flow

This step retrofits the phase 2-5 R build script to the four-phase
store-aware pattern described in dep-mgmt.md § Store-Aware Build Flow.
The only change from phase 2-5 is the insertion of store phases 2–4
between `lockfile_create()` and `lockfile_install()`. Phases 2 and 4
are handled by the `by-builder` binary — R only calls pak.

### Four-phase build script

The full build script. This replaces the phase 2-5 build script
(step 8 of that document). R handles phases 1 and 3 (pak API calls);
the `by-builder` binary handles phases 2 and 4 (store operations).

```r
library(pak, lib.loc = "/pak")
Sys.setenv(PKG_CACHE_DIR = "/pak-cache")

# ── Read manifest and configure ──────────────────────────────────
manifest <- jsonlite::fromJSON("/app/manifest.json")

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
    vapply(manifest$repositories, `[[`, "", "Name")
  )
  options(repos = repo_urls)
}

# Derive refs.
record_to_ref <- function(rec) {
  if (!is.null(rec$RemotePkgRef)) return(rec$RemotePkgRef)
  switch(rec$Source,
    Repository =, Bioconductor = {
      prefix <- if (rec$Source == "Bioconductor") "bioc::" else ""
      paste0(prefix, rec$Package, "@", rec$Version)
    },
    GitHub = paste0(rec$RemoteUsername, "/", rec$RemoteRepo,
                    "@", rec$RemoteSha),
    GitLab = paste0("gitlab::", rec$RemoteUsername, "/",
                    rec$RemoteRepo, "@", rec$RemoteSha),
    git    = paste0("git::", rec$RemoteUrl),
    stop("Unsupported Source: ", rec$Source)
  )
}

if (!is.null(manifest$packages)) {
  refs <- vapply(manifest$packages, record_to_ref, "")
} else {
  refs <- "deps::/app"
}

# Build library lives on the store volume so ingestion (phase 4) is
# an atomic rename() — no cross-filesystem copy.
build_uuid <- Sys.getenv("BUILD_UUID")
build_lib <- file.path("/store", ".builds", build_uuid)
dir.create(build_lib, recursive = TRUE)

# ── Phase 1: Resolve + solve (no download, no install) ───────────
pak::lockfile_create(refs,
  lockfile = file.path(build_lib, "pak.lock"), lib = build_lib)

# ── Phase 2: Check store, pre-populate build library ─────────────
# by-builder reads the lockfile, computes store keys, checks the
# store, and hard-links hits into the build library.
system2("/tools/by-builder", c(
  "store", "populate",
  "--lockfile", file.path(build_lib, "pak.lock"),
  "--lib", build_lib,
  "--store", "/store"
))

# ── Phase 3: Install store misses ────────────────────────────────
# lockfile_install() scans build_lib (update=TRUE by default),
# finds pre-populated packages, marks them as installed (type =
# "installed"), and skips them — no download, no install. Only
# store misses are downloaded and installed.
pak::lockfile_install(file.path(build_lib, "pak.lock"), lib = build_lib)

# ── Phase 4: Ingest newly installed packages into store ──────────
# by-builder ingests new packages from the build library into the
# store, with locking and metadata.
system2("/tools/by-builder", c(
  "store", "ingest",
  "--lockfile", file.path(build_lib, "pak.lock"),
  "--lib", build_lib,
  "--store", "/store"
))
```

For full store hits, phase 3 is a no-op — the build completes in
seconds. The lockfile serves double duty: it drives both the store
lookup (phase 2) and the installation (phase 3).

### Container mounts (updated from phase 2-5)

```
/app              (ro)  ← bundle (unpacked, includes manifest.json)
/pak              (ro)  ← cached pak package
/pak-cache        (rw)  ← persistent pak download cache (shared across builds)
/store            (rw)  ← package store root (shared across all builds)
/tools/by-builder (ro)  ← cached by-builder binary
```

The per-bundle `/build-lib` mount from phase 2-5 is replaced by
`/store`. The build library is created inside the store volume at
`/store/.builds/{uuid}/`, ensuring that store ingestion (phase 4) is
an atomic `rename()` within the same filesystem.

`/tools/by-builder` is a single static Go binary — no dependencies,
no runtime. It shares `internal/pkgstore` with the server.

---

## Step 5: Build integration (Go side)

### Updated buildCommand

```go
// internal/bundle/restore.go

func buildCommand() []string {
    // The R script is embedded or written to the build container.
    // It handles both pinned and unpinned modes, calls by-builder for
    // store phases 2 and 4, and reads BUILD_UUID from the environment.
    rScript := `... (four-phase build script from step 5) ...`
    return []string{"R", "--vanilla", "-e", rScript}
}
```

The `BUILD_UUID` env var is injected into the build container so the R
script creates the build library at `/store/.builds/{uuid}/` and the Go
code knows where to find the lockfile afterward.

### Updated buildMounts

```go
func buildMounts(
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
```

Key change from phase 2-5: the per-bundle `/build-lib` mount is
replaced by the shared store mount at `/store`. The build library
directory is created by the R script inside the store volume.
`/tools/by-builder` is the cached Go binary for store operations.

### Updated restore flow

```go
func runRestore(p RestoreParams) error {
    p.DB.UpdateBundleStatus(p.BundleID, "building")
    p.Sender.Write("restoring dependencies...")

    // 1. Ensure pak and by-builder are cached.
    pakPath, err := pakcache.EnsureInstalled(
        context.Background(), p.Backend,
        p.Image, p.PakVersion, p.PakCachePath)
    if err != nil {
        return fmt.Errorf("ensure pak: %w", err)
    }
    builderPath, err := buildercache.EnsureCached(
        p.BuilderCachePath, p.BuilderVersion)
    if err != nil {
        return fmt.Errorf("ensure by-builder: %w", err)
    }

    // 2. Resolve manifest from bundle contents.
    m, err := resolveManifest(p.Paths.Unpacked)
    if err != nil {
        return fmt.Errorf("resolve manifest: %w", err)
    }

    // 3. Bare scripts: pre-process to generate DESCRIPTION, then retry.
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

    // 4. Write manifest.json to unpacked dir (if generated server-side).
    manifestPath := filepath.Join(p.Paths.Unpacked, "manifest.json")
    if !fileExists(manifestPath) {
        if err := m.Write(manifestPath); err != nil {
            return fmt.Errorf("write manifest: %w", err)
        }
    }

    mode := m.BuildMode()
    p.Sender.Write(fmt.Sprintf("build mode: %s", mode))

    // 5. Ensure download cache and store dirs exist.
    dlCachePath := filepath.Join(p.BasePath, ".pak-dl-cache")
    os.MkdirAll(dlCachePath, 0o755)
    os.MkdirAll(p.Store.Root(), 0o755)

    // 6. Generate build UUID for the build library path.
    buildUUID := uuid.New().String()

    // 7. Run build container.
    spec := backend.BuildSpec{
        AppID:    p.AppID,
        BundleID: p.BundleID,
        Image:    p.Image,
        Cmd:      buildCommand(),
        Mounts:   buildMounts(pakPath, p.Paths.Unpacked, p.Store.Root(), dlCachePath, builderPath),
        Env:      []string{"BUILD_UUID=" + buildUUID},
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
        return fmt.Errorf("dependency restore failed (exit %d)", result.ExitCode)
    }

    // 8. Extract lockfile from the build directory.
    buildDir := filepath.Join(p.Store.Root(), ".builds", buildUUID)
    defer os.RemoveAll(buildDir) // clean up temp build dir

    lockfileSrc := filepath.Join(buildDir, "pak.lock")
    lockfileDst := filepath.Join(p.Paths.Base, "pak.lock")
    if err := copyFile(lockfileSrc, lockfileDst); err != nil {
        return fmt.Errorf("persist pak lockfile: %w", err)
    }

    // 9. Set store platform from lockfile (first build bootstraps this).
    if p.Store.Platform() == "" {
        lf, err := pkgstore.ReadLockfile(lockfileDst)
        if err == nil {
            p.Store.SetPlatform(pkgstore.PlatformFromLockfile(lf))
        }
    }

    // 10. Persist manifest alongside bundle.
    manifestDst := filepath.Join(p.Paths.Base, "manifest.json")
    if err := m.Write(manifestDst); err != nil {
        slog.Warn("failed to persist manifest",
            "error", err, "bundle_id", p.BundleID)
    }

    // 11. Activate bundle.
    if err := p.DB.ActivateBundle(p.AppID, p.BundleID); err != nil {
        return fmt.Errorf("activate bundle: %w", err)
    }

    // 12. Enforce retention.
    bundle.EnforceRetention(p.DB, p.BasePath, p.AppID, p.BundleID, p.Retention)

    return nil
}
```

### RestoreParams changes

```go
type RestoreParams struct {
    Backend          backend.Backend
    DB               *db.DB
    Tasks            *task.Store
    Sender           task.Sender
    AppID            string
    BundleID         string
    Paths            Paths
    Image            string
    PakVersion       string
    PakCachePath     string
    BuilderVersion   string  // NEW: by-builder binary version
    BuilderCachePath string  // NEW: by-builder cache directory
    Retention        int
    BasePath         string
    Store            *pkgstore.Store  // NEW: package store
    AuditLog         *audit.Log
    AuditActor       string
}
```

### BuildSpec changes

`BuildSpec` gains an `Env` field for injecting environment variables
into build containers:

```go
type BuildSpec struct {
    // ... existing fields from phase 2-5 ...
    Env []string // environment variables (KEY=VALUE)
}
```

The Docker backend sets these via `ContainerConfig.Env`.

### Post-build artifacts

```
{bundle_server_path}/{app_id}/bundles/{bundle_id}/
├── unpacked/          # bundle contents (app.R, manifest.json, ...)
├── pak.lock           # pak lockfile — exact versions, sources, hashes
└── manifest.json      # canonical manifest (pinned or unpinned)
```

The per-bundle `library/` directory from phase 2-5 is no longer
needed. Workers assemble their libraries from the store at startup.
The build library is temporary (created under `{store}/.builds/` and
cleaned up after the lockfile is extracted).

---

## Step 6: Worker library assembly

At worker startup, the server assembles a per-container library at
`{store}/.workers/{worker_id}/` by hard-linking packages from the store
based on the bundle's pak lockfile. The directory is mounted into the
container at `/lib`. R runs with `.libPaths("/lib")`.

```go
// internal/pkgstore/assembly.go

// AssembleLibrary creates a library directory by hard-linking packages
// from the store based on pak lockfile entries. Each lockfile entry maps
// to a store path via the curated hash. After linking, it writes a
// .packages.json manifest mapping each installed package to its store key.
func (s *Store) AssembleLibrary(
    libDir string, entries []LockfileEntry,
) (missing []string, err error) {
    if err := os.MkdirAll(libDir, 0o755); err != nil {
        return nil, fmt.Errorf("create lib dir: %w", err)
    }

    manifest := make(map[string]string) // package → store_key

    for _, entry := range entries {
        key, err := StoreKey(entry)
        if err != nil {
            return nil, fmt.Errorf("store key for %s: %w", entry.Package, err)
        }

        storePath := s.Path(entry.Package, key)
        if !dirExists(storePath) {
            missing = append(missing, entry.Package)
            continue
        }

        destPath := filepath.Join(libDir, entry.Package)
        // cp -al: hard-link the package tree.
        out, cpErr := exec.Command(
            "cp", "-al", storePath, destPath,
        ).CombinedOutput()
        if cpErr != nil {
            return nil, fmt.Errorf(
                "hard-link %s: %s: %w", entry.Package, out, cpErr)
        }

        // Touch metadata to update last-accessed time.
        s.Touch(entry.Package, key)
        manifest[entry.Package] = key
    }

    // Write per-worker package manifest.
    if err := WritePackageManifest(libDir, manifest); err != nil {
        return nil, fmt.Errorf("write package manifest: %w", err)
    }

    return missing, nil
}

// WorkerLibDir returns the host-side library directory for a worker.
func (s *Store) WorkerLibDir(workerID string) string {
    return filepath.Join(s.root, ".workers", workerID)
}

// CleanupWorkerLib removes a worker's library directory.
func (s *Store) CleanupWorkerLib(workerID string) error {
    dir := s.WorkerLibDir(workerID)
    if _, err := os.Stat(dir); os.IsNotExist(err) {
        return nil
    }
    return os.RemoveAll(dir)
}
```

### Per-worker package manifest

Each worker's `/lib` contains a `.packages.json` file that maps every
installed package to its store key. This is the source of truth for
what's actually installed in that specific worker — needed because live
installs (phase 2-7) can add packages beyond the app-level lockfile.

```json
{
  "shiny": "e3b0c44298fc...",
  "ggplot2": "b5bb9d8088f8..."
}
```

`AssembleLibrary` writes the initial manifest; `UpdatePackageManifest`
appends entries when packages are installed at runtime.

```go
// internal/pkgstore/manifest.go

// packageManifestFile is the filename for the per-worker package manifest.
const packageManifestFile = ".packages.json"

// WritePackageManifest writes a per-worker package manifest to libDir.
func WritePackageManifest(libDir string, manifest map[string]string) error {
    data, err := json.MarshalIndent(manifest, "", "  ")
    if err != nil {
        return err
    }
    return os.WriteFile(filepath.Join(libDir, packageManifestFile), data, 0o644)
}

// ReadPackageManifest reads the per-worker package manifest.
func ReadPackageManifest(libDir string) (map[string]string, error) {
    data, err := os.ReadFile(filepath.Join(libDir, packageManifestFile))
    if err != nil {
        return nil, err
    }
    var manifest map[string]string
    return manifest, json.Unmarshal(data, &manifest)
}

// UpdatePackageManifest adds entries to the per-worker package manifest.
// Existing entries are preserved; additions overwrite on key collision.
func UpdatePackageManifest(libDir string, additions map[string]string) error {
    manifest, err := ReadPackageManifest(libDir)
    if err != nil && !os.IsNotExist(err) {
        return err
    }
    if manifest == nil {
        manifest = make(map[string]string)
    }
    for pkg, key := range additions {
        manifest[pkg] = key
    }
    return WritePackageManifest(libDir, manifest)
}
```

A single mutable library is simpler than a dual read-only/read-write
split: no `.libPaths()` shadowing semantics, no question about which
library a package lives in, and runtime additions (phase 2-7) go into
the same directory. See dep-mgmt.md § Design Decisions.

---

## Step 7: Worker lifecycle integration

### WorkerSpec changes

```go
// internal/backend/backend.go

type WorkerSpec struct {
    // ... existing fields ...
    LibDir string // server-side path to per-worker lib dir; empty if no store
}
```

`LibDir` replaces the previous `LibraryPath` (per-bundle library) with
a per-worker library (assembled from the store).

### WorkerMounts

Extend `WorkerMounts` to include the assembled library:

```go
func (mc MountConfig) WorkerMounts(
    bundlePath, libDir, workerMount string,
) (binds []string, mounts []mount.Mount) {
    // Bundle mount (read-only).
    // ... existing logic ...

    // Library mount (read-write — writable for runtime additions in phase 2-7).
    if libDir != "" {
        // same translation logic as other mounts (Volume/Bind/Native)
    }

    return binds, mounts
}
```

### R_LIBS

Update `R_LIBS` in `createWorkerContainer` to use the single library:

```go
"R_LIBS=/lib"
```

One library, one search path. When `/lib` is empty (no store), R sees
no packages — the same as a fresh install.

### Spawn integration

In `spawnWorker()`, assemble the library from the store before starting
the container:

```go
var libDir string
if srv.PkgStore != nil {
    wid := workerID
    libDir = srv.PkgStore.WorkerLibDir(wid)

    // Read the bundle's pak lockfile.
    lockfilePath := filepath.Join(bundlePaths.Base, "pak.lock")
    lf, err := pkgstore.ReadLockfile(lockfilePath)
    if err != nil {
        return "", "", fmt.Errorf("read pak lockfile: %w", err)
    }

    // Assemble library from store.
    missing, err := srv.PkgStore.AssembleLibrary(libDir, lf.Packages)
    if err != nil {
        return "", "", fmt.Errorf("assemble library: %w", err)
    }
    if len(missing) > 0 {
        slog.Warn("worker library: missing store entries",
            "worker_id", wid, "missing", missing)
    }
}

spec := backend.WorkerSpec{
    // ... existing fields ...
    LibDir: libDir,
}
```

### Eviction cleanup

In `EvictWorker()`, clean up the worker's library after the container
stops:

```go
if srv.PkgStore != nil {
    if err := srv.PkgStore.CleanupWorkerLib(workerID); err != nil {
        slog.Warn("evict: failed to clean worker lib",
            "worker_id", workerID, "error", err)
    }
}
```

### Startup cleanup

Remove orphaned worker library directories from previous runs:

```go
if srv.PkgStore != nil {
    workersDir := filepath.Join(srv.PkgStore.Root(), ".workers")
    entries, _ := os.ReadDir(workersDir)
    for _, e := range entries {
        if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
            continue
        }
        if _, found := srv.Workers.Get(e.Name()); !found {
            os.RemoveAll(filepath.Join(workersDir, e.Name()))
        }
    }
}
```

---

## Step 8: Server struct changes

### Server struct

```go
type Server struct {
    // ... existing fields ...
    PkgStore *pkgstore.Store // nil when not available
}
```

### Initialization

At server startup, create the store with a default root path:

```go
storePath := filepath.Join(cfg.Docker.BundleServerPath, ".pkg-store")
os.MkdirAll(storePath, 0o755)
srv.PkgStore = pkgstore.NewStore(storePath)

// Recover platform from existing lockfiles or store directories.
if platform := recoverPlatform(storePath, cfg); platform != "" {
    srv.PkgStore.SetPlatform(platform)
}
```

`recoverPlatform()` scans the store root for existing platform
directories (e.g., `4.4-linux-x86_64/`) or reads a cached platform
file written after the first successful build.

---

## Step 9: Tests

### Unit tests

**Store key:**
- `TestStoreKey_Standard` — `standard` type: includes package, version,
  sha256.
- `TestStoreKey_GitHub` — `github` type: includes package, RemoteSha,
  RemoteSubdir.
- `TestStoreKey_GitHubSubdir` — subdir included in hash input.
- `TestStoreKey_UnsupportedType` — `url` type returns error.
**Store operations:**
- `TestStoreHas` — empty store returns false; after Ingest returns true.
- `TestStoreIngest` — creates entry at correct path; source directory
  no longer exists (rename).
- `TestStoreIngestIdempotent` — second Ingest is a no-op.
- `TestStorePath` — format: `{root}/{platform}/{package}/{hash}`.
- `TestStoreMetaPath` — format: `{root}/{platform}/{package}/{hash}.json`.
- `TestStoreTouch` — mtime of metadata file updated.

**Store metadata:**
- `TestWriteReadStoreMeta` — round-trip.
- `TestStoreMetaWithLinkingTo` — linkingto map preserved.

**Lockfile parsing:**
- `TestReadLockfile` — valid pak lockfile JSON.
- `TestPlatformFromLockfile` — `"4.4.2"` → `"4.4-linux-x86_64"`.

**Library assembly:**
- `TestAssembleLibrary` — creates lib dir with hard-linked packages.
- `TestAssembleLibraryMissing` — missing store entries returned, others
  still linked.
- `TestAssembleLibraryEmpty` — empty lockfile produces empty lib dir.

**Worker lib management:**
- `TestWorkerLibDir` — format: `{root}/.workers/{worker_id}`.
- `TestCleanupWorkerLib` — removes directory.
- `TestCleanupWorkerLibNonexistent` — no error.

**by-builder CLI:**
- `TestPopulateCommand` — reads lockfile, hard-links store hits into
  build lib, skips misses.
- `TestPopulateWithReferenceLib` — packages in reference lib skipped.
- `TestPopulateABIMismatch` — source-compiled package with changed
  LinkingTo treated as miss.
- `TestIngestCommand` — ingests new packages under lock, writes metadata.
- `TestIngestIdempotent` — second ingest is a no-op (lock + re-check).
- `TestIngestConcurrent` — two ingest processes for the same package,
  only one writes.

### Integration tests

- **Store-aware build (full cache miss):** deploy app with no store
  entries → build runs all 4 phases → verify packages ingested into
  store → verify lockfile persisted.
- **Store-aware build (full cache hit):** deploy same app again (same
  manifest) → verify phase 3 is no-op (no packages downloaded or
  installed) → build completes in seconds.
- **Store-aware build (partial hit):** deploy app that shares some
  packages with a prior build → verify shared packages are store hits,
  new packages installed and ingested.
- **Concurrent builds:** two builds needing the same package → verify
  only one installs it, the other waits and uses the store entry.
- **Worker library assembly:** deploy app → spawn worker → verify
  `/lib` populated from store via hard links → verify R can load
  packages.
- **Worker eviction cleanup:** spawn worker → evict → verify worker
  lib directory removed.
- **ABI safety:** ingest source-compiled package with LinkingTo
  metadata → change linked dependency version → verify store hit
  is skipped (treated as miss).

### E2E tests

- Deploy app → build populates store → spawn worker → worker library
  assembled from store → app runs correctly.
- Second deploy of same app → build uses full store hits → verify
  faster build time.
- Delete store → re-deploy → verify clean build works.

---

## Design Decisions

1. **Build library inside the store volume.** The build library is
   created at `/store/.builds/{uuid}/` rather than a separate per-bundle
   directory. This ensures all file operations (hard-link pre-population,
   atomic rename for ingestion) happen within a single filesystem. No
   cross-filesystem copies needed.

2. **Lockfile extraction, not library persistence.** Phase 2-5 persists
   the installed library alongside the bundle and mounts it directly
   into workers. Phase 2-6 discards the build library after extracting
   the lockfile. Workers reconstruct their library from the store on
   demand. This decouples worker libraries from build artifacts — any
   worker's library can be reconstructed from its lockfile at any time,
   and the store serves as the single source of truth.

3. **`mkdir` for locking, not `flock`.** `flock()` is not reliable
   across all NFS implementations and Docker volume drivers. `mkdir` is
   atomic on all POSIX filesystems. Stale lock detection uses a simple
   age threshold (30 minutes) rather than PID checks, which don't work
   across container PID namespaces.

4. **All store operations in Go via `by-builder`.** Store checks,
   ingestion, locking, ABI checks, and metadata are all implemented
   in Go (`internal/pkgstore`) and run inside build containers via the
   `by-builder` binary. R scripts only call pak APIs (`lockfile_create`,
   `lockfile_install`) and shell out to `by-builder` for store phases.
   This eliminates the cross-language parity risk for `store_key()` —
   the hash function exists only in Go. Worker library assembly also
   uses the same Go package on the host side.

5. **`by-builder` binary, not a second container.** Store operations
   run as a mounted Go binary inside the build container rather than
   in a separate container invocation. This keeps the build as a
   single container lifecycle (no extra startup overhead) while keeping
   R scripts minimal. The binary is cross-compiled at release time for
   `linux/amd64` and `linux/arm64`, cached on the server (same pattern
   as pak), and selected at runtime via `runtime.GOARCH`.

6. **Platform prefix from the start.** The store key includes
   `{R_major}.{R_minor}-linux-{arch}` even though the server currently
   runs a single R version. This avoids migration when user-supplied
   build images or multi-arch support are added. See dep-mgmt.md
   § Platform-aware store key.

7. **Worker libraries under the store root.** Per-worker library
   directories live at `{store}/.workers/{worker_id}/` to guarantee
   same-filesystem placement for hard links. The alternative — a
   separate directory under `bundle_server_path` — would require
   ensuring both paths are on the same filesystem, adding a
   configuration constraint.

8. **No per-bundle library directory.** The `library/` subdirectory
   from phase 2-5 is removed. The lockfile is the portable record;
   the library is reconstructed on demand from the store. This reduces
   disk usage (packages stored once in the store, not once per bundle
   plus once in the store) and simplifies bundle cleanup.
