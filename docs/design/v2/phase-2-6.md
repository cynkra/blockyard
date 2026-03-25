# Phase 2-6: Package Store & Worker Library Assembly

A server-level content-addressable package store populated during builds
and consumed at worker startup. The store caches installed R packages
keyed by a curated hash of identity fields from the pak lockfile. Build
libraries are pre-populated from the store; workers assemble their
libraries via hard links. This phase retrofits the phase 2-5 build flow
to the store-aware four-phase pattern described in dep-mgmt.md.

Depends on phase 2-5 (manifest types, pak build pipeline, lockfile
output). **Breaking change:** bundles built under phase 2-5 must be
redeployed after phase 2-6 is deployed. The per-bundle `library/`
directory from 2-5 is no longer used — workers assemble from the
store, which has no entries for pre-2-6 bundles.

See [dep-mgmt.md](../dep-mgmt.md) for store key design, ABI safety
rationale, concurrency protocol, store layout, and design rationale.
This document covers how to build the store and integrate it with the
build pipeline and worker lifecycle.

## Deliverables

1. **Package store** (`internal/pkgstore/store.go`) — content-addressable
   directory with two-level keying:
   `{platform}/{package}/{source_hash}/{config_hash}`. The `platform`
   prefix encodes R version (minor), OS, and architecture (e.g.,
   `4.4-linux-x86_64`). Source hash is SHA-256 of selected identity
   fields from the pak lockfile entry. Config hash is SHA-256 of the
   sorted `LinkingTo` dependency store keys (canonical empty hash for
   packages without `LinkingTo`).
2. **Store operations** — `Has(pkg, sourceHash, configHash)`,
   `Path(pkg, sourceHash, configHash)`,
   `Ingest(pkg, sourceHash, configHash, src)`.
   Ingestion uses atomic `rename()` from a build directory on the same
   filesystem. Append-only — packages are never modified after insertion.
3. **Store concurrency** — file-based locking under
   `{root}/.locks/{platform}/{package}/{source_hash}.lock`. Concurrent
   builds wait with backoff for the lock holder to finish. Stale lock
   detection via age threshold.
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
│   │   └── e3b0c442.../         ← source hash (v1.9.1)
│   │       ├── configs.json     ← source-level metadata + config map
│   │       ├── a1b2c3d4.../     ← config: installed package tree
│   │       └── a1b2c3d4....json ← config sidecar (created_at)
│   ├── sf/
│   │   └── 7d865e95.../         ← source hash (v1.0.0)
│   │       ├── configs.json
│   │       ├── b5bb9d80.../     ← config A (Rcpp@key1)
│   │       ├── b5bb9d80....json
│   │       ├── c6cc0a91.../     ← config B (Rcpp@key2)
│   │       └── c6cc0a91....json
│   ├── ggplot2/
│   │   └── b5bb9d80.../
│   │       ├── configs.json
│   │       ├── e3b0c442.../     ← canonical empty config (no LinkingTo)
│   │       └── e3b0c442....json
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
            └── e3b0c442.lock    ← one lock per source hash
```

Each `{config_hash}/` directory contains the installed package tree
directly (`DESCRIPTION`, `R/`, `Meta/`, etc. — no nested package name
directory). The sibling `{config_hash}.json` file holds per-config
metadata. The `configs.json` file at the source-hash level records
source-level properties and the config map.

All helper directories (`.builds/`, `.workers/`, `.locks/`) live under
the store root to guarantee same-filesystem placement for hard links
and atomic `rename()`.

### LockfileEntry

Parsed from the pak lockfile (JSON). The curated hash and worker
library assembly are computed from these fields.

**NOTE:** The JSON field names below (`package`, `version`,
`RemoteType`, `sha256`, `RemoteSha`, `RemoteSubdir`) must be verified
against the actual output of `pak::lockfile_create()` before
implementation. Preliminary inspection of the actual pak lockfile
output shows:

- `metadata.RemoteType` (nested under `metadata`, not top-level)
- `metadata.RemoteSha` (nested; for standard type it's the version string)
- `type` at top level (value: `"standard"`)
- `needscompilation` (boolean, not string)
- `sha256` at top level (matches)
- `rversion` per-package (short form like `"4.5"`)
- `platform` per-package

The struct below preserves the current field mapping for clarity but
needs to be aligned with the actual nested format during
implementation. The `RemoteType`/`RemoteSha`/`RemoteSubdir` fields
will likely come from `metadata.*` paths when unmarshaling.

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
that determine what source code was compiled. See dep-mgmt.md § Package
Store and Cache Key Design for the full rationale.

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
// SourceDir returns the source-hash directory for a package.
func (s *Store) SourceDir(pkg, sourceHash string) string {
    return filepath.Join(s.root, s.platform, pkg, sourceHash)
}

// Path returns the config directory (installed package tree) path.
func (s *Store) Path(pkg, sourceHash, configHash string) string {
    return filepath.Join(s.root, s.platform, pkg, sourceHash, configHash)
}

// ConfigsPath returns the path to configs.json for a source hash.
func (s *Store) ConfigsPath(pkg, sourceHash string) string {
    return filepath.Join(s.root, s.platform, pkg, sourceHash, "configs.json")
}

// ConfigMetaPath returns the config sidecar file path.
func (s *Store) ConfigMetaPath(pkg, sourceHash, configHash string) string {
    return filepath.Join(s.root, s.platform, pkg, sourceHash, configHash+".json")
}

// Has reports whether the store contains a specific config for a package.
func (s *Store) Has(pkg, sourceHash, configHash string) bool {
    _, err := os.Stat(s.Path(pkg, sourceHash, configHash))
    return err == nil
}

// Ingest atomically moves an installed package tree into the store
// as a config entry. No-op if the config already exists. srcDir must
// be on the same filesystem as the store (for atomic rename).
func (s *Store) Ingest(pkg, sourceHash, configHash, srcDir string) error {
    dst := s.Path(pkg, sourceHash, configHash)
    if dirExists(dst) {
        return nil // already in store
    }
    if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
        return fmt.Errorf("create store dir: %w", err)
    }
    return os.Rename(srcDir, dst)
}

// Touch updates the mtime of a config's sidecar file.
// Used for last-accessed tracking (LRU eviction, future concern).
func (s *Store) Touch(pkg, sourceHash, configHash string) {
    metaPath := s.ConfigMetaPath(pkg, sourceHash, configHash)
    now := time.Now()
    os.Chtimes(metaPath, now, now)
}
```

### Store metadata

The store uses two metadata files at different levels:

**`configs.json`** — one per source hash. Records source-level
properties (invariant across configs) and maps config hashes to
their LinkingTo store key sets. Updated (under lock) when new
configs are ingested.

**`{config_hash}.json`** — one per config. Contains only `created_at`.
Its filesystem `mtime` serves as last-accessed tracking (`touch`ed on
every store hit).

```go
// internal/pkgstore/meta.go

// StoreConfigs represents the configs.json file at the source-hash level.
type StoreConfigs struct {
    SourceCompiled bool                         `json:"source_compiled"`
    LinkingTo      []string                     `json:"linkingto"`
    Configs        map[string]map[string]string `json:"configs"`
    // Configs key: config hash
    // Configs value: map of {linked_package: store_key}
    //   (empty map for packages without LinkingTo deps)
}

// ConfigMeta represents the per-config sidecar file.
type ConfigMeta struct {
    CreatedAt time.Time `json:"created_at"`
}

func WriteStoreConfigs(path string, sc StoreConfigs) error {
    data, err := json.MarshalIndent(sc, "", "  ")
    if err != nil {
        return err
    }
    return os.WriteFile(path, data, 0o644)
}

func ReadStoreConfigs(path string) (StoreConfigs, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return StoreConfigs{}, err
    }
    var sc StoreConfigs
    return sc, json.Unmarshal(data, &sc)
}

func WriteConfigMeta(path string, meta ConfigMeta) error {
    data, err := json.MarshalIndent(meta, "", "  ")
    if err != nil {
        return err
    }
    return os.WriteFile(path, data, 0o644)
}
```

| File | Field | Description |
|---|---|---|
| `configs.json` | `source_compiled` | `true` if the package was compiled from source (`NeedsCompilation: yes` in DESCRIPTION). Invariant across configs for the same source. |
| `configs.json` | `linkingto` | Sorted list of `LinkingTo` package names from the source DESCRIPTION. Empty for packages without `LinkingTo`. |
| `configs.json` | `configs` | Map of `{config_hash: {linked_pkg: store_key, ...}}`. Each entry is a distinct compilation. |
| `{config_hash}.json` | `created_at` | Timestamp written once at ingestion. |

**Last-accessed tracking** uses the config sidecar file's filesystem
`mtime` — the server `touch`es it on every store hit. `touch` is a
metadata-only syscall (`utimes()`), avoiding JSON rewrites on every
access. Eviction operates at the config level.

### ConfigHash

Computes the config hash from a map of LinkingTo package names to
their store keys. The hash is deterministic: entries are sorted by
package name, joined with `\x00`, and hashed with SHA-256. For
packages without `LinkingTo` deps, the input is empty and the result
is the canonical empty config hash.

```go
// internal/pkgstore/key.go

// ConfigHash computes the config hash from a LinkingTo dependency map.
// Entries are sorted by package name to ensure deterministic output.
// An empty map produces the canonical empty config hash.
func ConfigHash(linkingToKeys map[string]string) string {
    if len(linkingToKeys) == 0 {
        h := sha256.Sum256([]byte(""))
        return hex.EncodeToString(h[:])
    }

    // Sort by package name for determinism.
    pkgs := make([]string, 0, len(linkingToKeys))
    for pkg := range linkingToKeys {
        pkgs = append(pkgs, pkg)
    }
    sort.Strings(pkgs)

    var parts []string
    for _, pkg := range pkgs {
        parts = append(parts, pkg+"\x00"+linkingToKeys[pkg])
    }
    input := strings.Join(parts, "\x00")
    h := sha256.Sum256([]byte(input))
    return hex.EncodeToString(h[:])
}
```

### ResolveConfig

At populate time, looks up `configs.json` for a source hash, computes
the expected config hash from the current lockfile's store keys for
the package's `LinkingTo` dependencies, and returns the matching config
hash if it exists. This replaces the old `LinkingToMatches` approach.

```go
// internal/pkgstore/meta.go

// ResolveConfig reads configs.json for a package's source hash and
// returns the config hash that matches the current lockfile's
// LinkingTo store keys. Returns ("", false) if no matching config
// exists (miss) or if configs.json doesn't exist (never seen).
func (s *Store) ResolveConfig(
    pkg, sourceHash string, lf *Lockfile,
) (configHash string, ok bool) {
    sc, err := ReadStoreConfigs(s.ConfigsPath(pkg, sourceHash))
    if err != nil {
        return "", false // no configs.json — never ingested
    }

    // Compute expected config hash from current lockfile.
    linkingToKeys := make(map[string]string)
    for _, linkedPkg := range sc.LinkingTo {
        key := lockfileStoreKey(lf, linkedPkg)
        if key != "" {
            linkingToKeys[linkedPkg] = key
        }
    }
    expected := ConfigHash(linkingToKeys)

    if _, exists := sc.Configs[expected]; exists {
        return expected, true
    }
    return "", false
}
```

### WriteIngestMeta

Writes or updates `configs.json` and writes the config sidecar after
ingesting a package. Reads the installed DESCRIPTION to determine
`NeedsCompilation` and `LinkingTo`, computes the config hash from the
lockfile's store keys for the linked packages, and adds the config
entry.

```go
// internal/pkgstore/meta.go

// WriteIngestMeta writes the config sidecar and updates configs.json
// for a newly ingested package config.
func (s *Store) WriteIngestMeta(
    entry LockfileEntry, lf *Lockfile,
    sourceHash, configHash string, linkingToKeys map[string]string,
    sourceCompiled bool, linkingToNames []string,
) error {
    // Write or update configs.json.
    configsPath := s.ConfigsPath(entry.Package, sourceHash)
    sc, err := ReadStoreConfigs(configsPath)
    if err != nil {
        // First config for this source hash — create configs.json.
        sc = StoreConfigs{
            SourceCompiled: sourceCompiled,
            LinkingTo:      linkingToNames,
            Configs:        make(map[string]map[string]string),
        }
    }
    sc.Configs[configHash] = linkingToKeys
    if err := WriteStoreConfigs(configsPath, sc); err != nil {
        return fmt.Errorf("write configs.json: %w", err)
    }

    // Write config sidecar.
    meta := ConfigMeta{CreatedAt: time.Now()}
    metaPath := s.ConfigMetaPath(entry.Package, sourceHash, configHash)
    return WriteConfigMeta(metaPath, meta)
}

// IngestContext extracts compile-time context from an installed
// package's DESCRIPTION and computes the config hash from the
// lockfile. Returns the config hash, LinkingTo store key map,
// source_compiled flag, and sorted LinkingTo package names.
func IngestContext(
    descPath string, lf *Lockfile,
) (configHash string, linkingToKeys map[string]string,
    sourceCompiled bool, linkingToNames []string, err error) {

    desc, err := ParseDCF(descPath)
    if err != nil {
        // No DESCRIPTION — use empty config (shouldn't happen for
        // a successfully installed package, but safe fallback).
        return ConfigHash(nil), nil, false, nil, nil
    }

    sourceCompiled = strings.EqualFold(desc["NeedsCompilation"], "yes")
    linkingToKeys = make(map[string]string)

    if lt := desc["LinkingTo"]; lt != "" {
        linkingToNames = parsePkgList(lt)
        sort.Strings(linkingToNames)
        for _, linkedPkg := range linkingToNames {
            key := lockfileStoreKey(lf, linkedPkg)
            if key != "" {
                linkingToKeys[linkedPkg] = key
            }
        }
    }

    configHash = ConfigHash(linkingToKeys)
    return configHash, linkingToKeys, sourceCompiled, linkingToNames, nil
}

// ParseDCF reads a Debian Control File (DESCRIPTION) into a map.
func ParseDCF(path string) (map[string]string, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err
    }
    result := make(map[string]string)
    var currentKey, currentVal string
    for _, line := range strings.Split(string(data), "\n") {
        if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
            currentVal += " " + strings.TrimSpace(line)
        } else if idx := strings.IndexByte(line, ':'); idx > 0 {
            if currentKey != "" {
                result[currentKey] = strings.TrimSpace(currentVal)
            }
            currentKey = strings.TrimSpace(line[:idx])
            currentVal = strings.TrimSpace(line[idx+1:])
        }
    }
    if currentKey != "" {
        result[currentKey] = strings.TrimSpace(currentVal)
    }
    return result, nil
}

// parsePkgList splits a comma-separated DESCRIPTION field (e.g.,
// "Rcpp (>= 1.0.0), s2") into bare package names.
func parsePkgList(s string) []string {
    var result []string
    for _, part := range strings.Split(s, ",") {
        name := strings.TrimSpace(part)
        if idx := strings.IndexByte(name, '('); idx > 0 {
            name = strings.TrimSpace(name[:idx])
        }
        if name != "" {
            result = append(result, name)
        }
    }
    return result
}

// lockfileStoreKey computes the store key for a named package from
// the lockfile, returning "" if the package is not found (e.g., base
// R packages like methods).
func lockfileStoreKey(lf *Lockfile, pkg string) string {
    for _, entry := range lf.Packages {
        if entry.Package == pkg {
            key, err := StoreKey(entry)
            if err != nil {
                return ""
            }
            return key
        }
    }
    return ""
}
```

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
   `{root}/.locks/{platform}/{package}/{source_hash}.lock`. The lock
   is a directory (created atomically via `mkdir`).
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

func (s *Store) LockPath(pkg, sourceHash string) string {
    return filepath.Join(s.root, ".locks", s.platform, pkg, sourceHash+".lock")
}

func (s *Store) Acquire(pkg, sourceHash string, staleThreshold time.Duration) error {
    lockDir := s.LockPath(pkg, sourceHash)
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

func (s *Store) Release(pkg, sourceHash string) {
    os.RemoveAll(s.LockPath(pkg, sourceHash))
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

Reads the pak lockfile, checks the store for each entry by computing
the source hash and resolving the matching config hash via
`configs.json`, then hard-links hits into the build library. Packages
already present in the build library (or in an optional reference
library) are skipped.

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

            // Load the reference library's package manifest to compare
            // store keys, not just directory existence. A package in the
            // reference lib with a DIFFERENT store key (version change)
            // should still be looked up in the store.
            var refManifest map[string]string
            if refLib != "" {
                refManifest, _ = pkgstore.ReadPackageManifest(refLib)
            }

            var hits, misses int
            for _, entry := range lf.Packages {
                sourceHash, err := pkgstore.StoreKey(entry)
                if err != nil {
                    return err
                }

                // Skip packages whose source hash matches the reference
                // library — same version already installed in the worker.
                if refManifest != nil && refManifest[entry.Package] == sourceHash {
                    continue
                }
                // Skip packages already in the build/staging library.
                if dirExists(filepath.Join(lib, entry.Package)) {
                    continue
                }

                // Resolve matching config via configs.json.
                // ResolveConfig reads the LinkingTo package names from
                // configs.json, looks up their store keys in the current
                // lockfile, computes the expected config hash, and checks
                // if it exists.
                configHash, ok := s.ResolveConfig(entry.Package, sourceHash, lf)
                if !ok {
                    misses++
                    continue
                }

                // Hard-link config's package tree into build library.
                // cp -al creates a recursive hard-link copy — every file
                // in the source tree gets a hard link in the destination.
                dest := filepath.Join(lib, entry.Package)
                out, cpErr := exec.Command(
                    "cp", "-al",
                    s.Path(entry.Package, sourceHash, configHash), dest,
                ).CombinedOutput()
                if cpErr != nil {
                    return fmt.Errorf("link %s: %s: %w", entry.Package, out, cpErr)
                }
                s.Touch(entry.Package, sourceHash, configHash)
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
For each package, reads the installed DESCRIPTION to determine the
config hash, then ingests under lock. Packages already in the store
with a matching config are skipped. Locking ensures concurrent builds
don't conflict.

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

            // Load the reference library's package manifest to compare
            // source hashes. Only skip ingestion when the reference lib
            // has the SAME source hash — a version change means the new
            // version was installed in the staging dir and must be ingested.
            var refManifest map[string]string
            if refLib != "" {
                refManifest, _ = pkgstore.ReadPackageManifest(refLib)
            }

            for _, entry := range lf.Packages {
                sourceHash, err := pkgstore.StoreKey(entry)
                if err != nil {
                    return err
                }

                // Skip packages whose source hash matches the reference
                // library — already in the store from a prior build.
                if refManifest != nil && refManifest[entry.Package] == sourceHash {
                    continue
                }

                pkgPath := filepath.Join(lib, entry.Package)
                if !dirExists(pkgPath) {
                    continue // not installed (shouldn't happen)
                }

                // Compute config hash from installed DESCRIPTION.
                descPath := filepath.Join(pkgPath, "DESCRIPTION")
                configHash, linkingToKeys, sourceCompiled, linkingToNames, err :=
                    pkgstore.IngestContext(descPath, lf)
                if err != nil {
                    return fmt.Errorf("ingest context for %s: %w",
                        entry.Package, err)
                }

                if s.Has(entry.Package, sourceHash, configHash) {
                    continue // this exact config already in store
                }

                // Ingest under lock (one lock per source hash).
                s.Acquire(entry.Package, sourceHash, 30*time.Minute)
                if !s.Has(entry.Package, sourceHash, configHash) {
                    s.Ingest(entry.Package, sourceHash, configHash, pkgPath)
                    s.WriteIngestMeta(entry, lf,
                        sourceHash, configHash, linkingToKeys,
                        sourceCompiled, linkingToNames)
                }
                s.Release(entry.Package, sourceHash)

                fmt.Fprintf(os.Stderr, "store: ingested %s (config %s)\n",
                    entry.Package, configHash[:12])
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
# by-builder reads the lockfile, computes source hashes, reads
# configs.json for each package, resolves the matching config hash,
# and hard-links config hits into the build library.
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
# by-builder reads installed DESCRIPTIONs, computes config hashes,
# creates config directories, updates configs.json, and writes
# config sidecar files — all under lock.
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

## Step 6: Build integration (Go side)

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

## Step 7: Worker library assembly

At worker startup, the server assembles a per-container library at
`{store}/.workers/{worker_id}/` by hard-linking packages from the store
based on the bundle's pak lockfile. The directory is mounted into the
container at `/lib`. R runs with `.libPaths("/lib")`.

```go
// internal/pkgstore/assembly.go

// AssembleLibrary creates a library directory by hard-linking packages
// from the store based on pak lockfile entries. Each lockfile entry maps
// to a source hash, then configs.json is consulted to find the matching
// config hash. After linking, it writes a .packages.json manifest
// mapping each installed package to its source hash (store key).
func (s *Store) AssembleLibrary(
    libDir string, lf *Lockfile,
) (missing []string, err error) {
    if err := os.MkdirAll(libDir, 0o755); err != nil {
        return nil, fmt.Errorf("create lib dir: %w", err)
    }

    manifest := make(map[string]string) // package → source_hash

    for _, entry := range lf.Packages {
        sourceHash, err := StoreKey(entry)
        if err != nil {
            return nil, fmt.Errorf("store key for %s: %w", entry.Package, err)
        }

        // Resolve matching config from configs.json.
        configHash, ok := s.ResolveConfig(entry.Package, sourceHash, lf)
        if !ok {
            missing = append(missing, entry.Package)
            continue
        }

        storePath := s.Path(entry.Package, sourceHash, configHash)
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

        // Touch config sidecar to update last-accessed time.
        s.Touch(entry.Package, sourceHash, configHash)
        manifest[entry.Package] = sourceHash
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

## Step 8: Worker lifecycle integration

### WorkerSpec changes

```go
// internal/backend/backend.go

type WorkerSpec struct {
    // ... existing fields ...
    LibDir      string // server-side path to per-worker lib dir; empty if no store
    TransferDir string // server-side path to per-worker transfer dir (phase 2-7)
}
```

`LibDir` replaces the previous `LibraryPath` (per-bundle library) with
a per-worker library (assembled from the store).

`TransferDir` is a pre-created directory for container transfer
signaling (phase 2-7). Mounted read-write into every worker at
`/transfer` so the R session can write board state there if a version
conflict requires a container swap. Empty for most workers' lifetime.

### WorkerMounts

Extend `WorkerMounts` to include the assembled library and transfer
directory:

```go
func (mc MountConfig) WorkerMounts(
    bundlePath, libDir, transferDir, workerMount string,
) (binds []string, mounts []mount.Mount) {
    // Bundle mount (read-only).
    // ... existing logic ...

    // Library mount (read-only from inside the container). Runtime
    // package additions (phase 2-7) are hardlinked from the host side
    // by the server — host-side writes are visible through the bind
    // mount regardless of the ro flag. Making it ro prevents
    // install.packages() from inside R, which is correct since
    // installations must go through the server's packages API.
    if libDir != "" {
        // same translation logic as other mounts (Volume/Bind/Native)
        // mount at /lib, read-only
    }

    // Transfer mount (read-write). Pre-created at spawn time for
    // container transfer signaling (phase 2-7). The R session writes
    // board.json here when a version conflict requires a container
    // swap. Empty until a transfer is triggered.
    if transferDir != "" {
        // mount at /transfer, read-write
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
    missing, err := srv.PkgStore.AssembleLibrary(libDir, lf)
    if err != nil {
        return "", "", fmt.Errorf("assemble library: %w", err)
    }
    if len(missing) > 0 {
        slog.Warn("worker library: missing store entries",
            "worker_id", wid, "missing", missing)
    }
}

// Pre-create transfer directory for container transfer signaling
// (phase 2-7). Mounted rw at /transfer inside the container.
// TransferDir returns {bundle_server_path}/.transfers/{worker_id}.
transferDir := srv.TransferDir(workerID)
os.MkdirAll(transferDir, 0o755)

spec := backend.WorkerSpec{
    // ... existing fields ...
    LibDir:      libDir,
    TransferDir: transferDir,
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
// Clean up transfer directory.
os.RemoveAll(srv.TransferDir(workerID))
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

## Step 9: Server struct changes

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

## Step 10: Tests

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
- `TestStorePath` — format:
  `{root}/{platform}/{package}/{source_hash}/{config_hash}`.
- `TestStoreConfigsPath` — format:
  `{root}/{platform}/{package}/{source_hash}/configs.json`.
- `TestStoreConfigMetaPath` — format:
  `{root}/{platform}/{package}/{source_hash}/{config_hash}.json`.
- `TestStoreTouch` — mtime of config sidecar updated.

**Config hash:**
- `TestConfigHash_Empty` — empty map produces canonical empty hash.
- `TestConfigHash_SingleDep` — deterministic for single LinkingTo dep.
- `TestConfigHash_MultipleDeps` — sorted by package name, deterministic.
- `TestConfigHash_OrderIndependent` — same deps in different order
  produce same hash.

**Store metadata:**
- `TestWriteReadStoreConfigs` — round-trip for configs.json.
- `TestStoreConfigsWithMultipleConfigs` — multiple config entries
  preserved.
- `TestWriteReadConfigMeta` — round-trip for config sidecar.

**Config resolution:**
- `TestResolveConfig_NoConfigsFile` — returns false when no
  configs.json exists.
- `TestResolveConfig_MatchingConfig` — returns correct config hash
  when LinkingTo store keys match.
- `TestResolveConfig_NoMatchingConfig` — returns false when LinkingTo
  store keys differ from all existing configs.
- `TestResolveConfig_EmptyLinkingTo` — matches canonical empty config
  hash.

**Ingest context:**
- `TestIngestContext_NoLinkingTo` — returns empty config hash,
  source_compiled from DESCRIPTION.
- `TestIngestContext_WithLinkingTo` — returns correct config hash,
  sorted linkingto names, store key map.

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
  LinkingTo config not found → treated as miss.
- `TestPopulateMultiConfig` — package with multiple configs, correct
  one selected based on current lockfile.
- `TestIngestCommand` — ingests new packages under lock, writes
  configs.json and config sidecar.
- `TestIngestIdempotent` — second ingest of same config is a no-op
  (lock + re-check).
- `TestIngestNewConfig` — second ingest of same source hash with
  different LinkingTo store keys adds new config entry.
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
- **ABI safety (multi-config):** ingest source-compiled package with
  LinkingTo deps → change linked dependency version → verify original
  config is not matched → pak recompiles → verify new config added
  to configs.json → verify both configs coexist in the store.

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
