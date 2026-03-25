# Phase 2-5: Manifest Format & pak Build Pipeline

Replace rv with pak as the build-time dependency manager. Introduce a
two-shape manifest format (pinned and unpinned) as the canonical interface
between CLI and server. The build pipeline consumes manifests, resolves
dependencies via pak, and persists the resulting pak lockfile for downstream
phases (store integration in phase 2-6, worker assembly and refresh in
phase 2-7). This phase implements the pipeline without store integration —
builds run `lockfile_create()` → `lockfile_install()` end-to-end through pak.

See [dep-mgmt.md](../dep-mgmt.md) for the full architectural overview,
manifest schemas, ref derivation rules, store key design, and design
rationale. This document covers how to build the pipeline.

## Deliverables

1. **Manifest types** (`internal/manifest/`) — Go types for both manifest
   shapes: pinned (with `packages`) and unpinned (with `description`).
   Shared envelope: `version`, `platform`, `metadata`, `repositories`,
   `files`. Validation rejects manifests carrying both `packages` and
   `description`. Schema version check (reject unknown versions).
2. **renv.lock → manifest conversion** — pure Go in `internal/manifest/`.
   Package records copy verbatim. Top-level mapping: `R.Version` →
   `platform`, `R.Repositories` → `repositories`.
3. **DESCRIPTION → unpinned manifest** — pure Go in `internal/manifest/`.
   Parses DCF fields and JSON-ifies them as string values into the
   `description` object.
4. **pak cache** (`internal/pakcache/`) — download and cache pak's
   pre-built package bundle, replacing `internal/rvcache/`. Same pattern:
   download once, cache locally, mount read-only into build containers.
5. **Build mode detection** — server dispatches on manifest contents:
   `packages` present → pinned; `description` present → unpinned; no
   manifest → bare-script pre-processing then unpinned.
6. **Build container R scripts** — ref derivation (`record_to_ref()`:
   renv-style package record → pkgdepends ref string), platform URL
   transformation (PPM neutral → platform-specific), and the build flow:
   `lockfile_create(refs)` → `lockfile_install()`. Pinned builds derive
   refs from package records; unpinned builds use `deps::/app`.
7. **Bare script pre-processing** — R script using
   `pkgdepends::scan_deps()` to discover dependencies from scripts,
   generate a synthetic DESCRIPTION, and build an unpinned manifest.
   Both artifacts persisted alongside the bundle.
8. **Post-build lockfile storage** — persist the pak lockfile alongside
   the bundle after successful builds. Drives worker library assembly
   (phase 2-6) and runtime requests (phase 2-7).
9. **BuildSpec extension** — add `Cmd` and `Mounts` fields to `BuildSpec`
   so the `Build` method supports flexible commands and mount
   configurations.
10. **Bundle validation** — relax lockfile requirement. Only `app.R` (or
    `server.R`/`ui.R`) is mandatory.
11. **Config changes** — replace `rv_version` with `pak_version`.
12. **Remove rv** — delete `internal/rvcache/`, `SetLibraryPath()`,
    `RvBinaryPath`. Update examples.

---

## Step 1: Manifest types

The manifest is the canonical interface between CLI and server. Both
shapes share an envelope (`version`, `platform`, `metadata`,
`repositories`, `files`) but differ in how they specify dependencies.
See dep-mgmt.md § Manifest Format for the full schema and rationale.

```go
// internal/manifest/manifest.go

type Manifest struct {
    Version      int                 `json:"version"`
    Platform     string              `json:"platform"`
    Metadata     Metadata            `json:"metadata"`
    Repositories []Repository        `json:"repositories"`
    Packages     map[string]Package  `json:"packages,omitempty"`
    Description  map[string]string   `json:"description,omitempty"`
    Files        map[string]FileInfo `json:"files"`
}

type Metadata struct {
    AppMode    string `json:"appmode"`
    Entrypoint string `json:"entrypoint"`
}

type Repository struct {
    Name string `json:"Name"`
    URL  string `json:"URL"`
}

// Package holds the renv.lock fields consumed by the build pipeline.
// Only identity and source fields are mapped — record_to_ref() uses
// Source/Remote* to derive pkgdepends refs. Fields like Hash,
// Requirements, and DESCRIPTION metadata (Title, Authors@R, etc.)
// are not consumed by any part of the pipeline and are not carried.
// Works with both renv.lock v1 (minimal) and v2 (full DESCRIPTION)
// formats — extra fields are silently dropped during unmarshaling.
type Package struct {
    Package        string `json:"Package"`
    Version        string `json:"Version"`
    Source         string `json:"Source"`
    Repository     string `json:"Repository,omitempty"`
    RemoteType     string `json:"RemoteType,omitempty"`
    RemoteHost     string `json:"RemoteHost,omitempty"`
    RemoteUsername string `json:"RemoteUsername,omitempty"`
    RemoteRepo     string `json:"RemoteRepo,omitempty"`
    RemoteRef      string `json:"RemoteRef,omitempty"`
    RemoteSha      string `json:"RemoteSha,omitempty"`
    RemoteSubdir   string `json:"RemoteSubdir,omitempty"`
    RemoteUrl      string `json:"RemoteUrl,omitempty"` // git:: sources
}

// Validate checks that a package record has the fields required by
// record_to_ref() for its Source type.
func (p Package) Validate() error {
    if p.Package == "" {
        return errors.New("missing Package field")
    }
    if p.Source == "" {
        return fmt.Errorf("package %s: missing Source field", p.Package)
    }

    switch p.Source {
    case "Repository", "Bioconductor":
        if p.Version == "" {
            return fmt.Errorf("package %s: Source %q requires Version",
                p.Package, p.Source)
        }
    case "GitHub", "GitLab":
        for _, f := range []struct{ name, val string }{
            {"RemoteUsername", p.RemoteUsername},
            {"RemoteRepo", p.RemoteRepo},
            {"RemoteSha", p.RemoteSha},
        } {
            if f.val == "" {
                return fmt.Errorf("package %s: Source %q requires %s",
                    p.Package, p.Source, f.name)
            }
        }
    case "git":
        if p.RemoteUrl == "" {
            return fmt.Errorf("package %s: Source \"git\" requires RemoteUrl",
                p.Package)
        }
    default:
        return fmt.Errorf("package %s: unsupported Source %q", p.Package, p.Source)
    }
    return nil
}

type FileInfo struct {
    Checksum string `json:"checksum"`
}
```

### IsPinned

```go
func (m *Manifest) IsPinned() bool { return len(m.Packages) > 0 }
```

### Validate

```go
const currentVersion = 1

func (m *Manifest) Validate() error {
    if m.Version != currentVersion {
        return fmt.Errorf(
            "unsupported manifest version %d (server supports %d)",
            m.Version, currentVersion)
    }
    if len(m.Packages) > 0 && len(m.Description) > 0 {
        return errors.New(
            "manifest carries both packages and description; " +
                "must be one or the other")
    }
    if m.Metadata.Entrypoint == "" {
        return errors.New("manifest missing metadata.entrypoint")
    }
    for _, pkg := range m.Packages {
        if err := pkg.Validate(); err != nil {
            return fmt.Errorf("invalid package record: %w", err)
        }
    }
    return nil
}
```

### Read / Write

```go
func Read(path string) (*Manifest, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err
    }
    var m Manifest
    if err := json.Unmarshal(data, &m); err != nil {
        return nil, fmt.Errorf("parse manifest: %w", err)
    }
    if err := m.Validate(); err != nil {
        return nil, fmt.Errorf("invalid manifest: %w", err)
    }
    return &m, nil
}

func (m *Manifest) Write(path string) error {
    data, err := json.MarshalIndent(m, "", "  ")
    if err != nil {
        return err
    }
    return os.WriteFile(path, data, 0o644)
}
```

---

## Step 2: renv.lock → manifest conversion

Pure Go. The entire conversion copies package records verbatim and maps
top-level renv.lock fields to the manifest envelope. See dep-mgmt.md
§ renv.lock → Manifest Translation for the field mapping.

```go
// internal/manifest/renvlock.go

// renvLock mirrors the relevant structure of renv.lock (JSON).
// Works with both v1 (minimal records) and v2 (full DESCRIPTION)
// lockfile formats — the Package struct maps only the fields we
// need; extra v2 fields are silently dropped during unmarshaling.
type renvLock struct {
    R struct {
        Version      string       `json:"Version"`
        Repositories []Repository `json:"Repositories"`
    } `json:"R"`
    Packages map[string]Package `json:"Packages"`
}

// FromRenvLock converts an renv.lock file to a pinned manifest.
// Package identity and source fields are preserved unchanged.
// Extra DESCRIPTION fields from v2 lockfiles are not carried.
func FromRenvLock(
    lockPath string,
    meta Metadata,
    files map[string]FileInfo,
) (*Manifest, error) {
    data, err := os.ReadFile(lockPath)
    if err != nil {
        return nil, fmt.Errorf("read renv.lock: %w", err)
    }

    var lock renvLock
    if err := json.Unmarshal(data, &lock); err != nil {
        return nil, fmt.Errorf("parse renv.lock: %w", err)
    }

    m := &Manifest{
        Version:      currentVersion,
        Platform:     lock.R.Version,
        Metadata:     meta,
        Repositories: lock.R.Repositories,
        Packages:     lock.Packages,
        Files:        files,
    }
    return m, m.Validate()
}
```

Package identity and source fields (`Version`, `Source`, `Repository`,
`RemoteType`, `RemoteUsername`, `RemoteRepo`, `RemoteRef`, `RemoteSha`,
`RemoteHost`, `RemoteSubdir`) pass through unchanged. Fields not consumed
by the pipeline (`Hash`, `Requirements`, DESCRIPTION metadata) are
silently dropped during unmarshaling.

---

## Step 3: DESCRIPTION → unpinned manifest

Pure Go. Parses the DESCRIPTION file as DCF (Debian Control Format) and
stores each field as a string value in the `description` map. No R needed.

```go
// internal/manifest/description.go

// FromDescription builds an unpinned manifest from a DESCRIPTION file.
// DCF fields are JSON-ified as string values into the description object.
func FromDescription(
    descPath string,
    meta Metadata,
    files map[string]FileInfo,
    repos []Repository,
) (*Manifest, error) {
    data, err := os.ReadFile(descPath)
    if err != nil {
        return nil, fmt.Errorf("read DESCRIPTION: %w", err)
    }

    fields := parseDCF(data)

    // Extract only dependency-relevant fields.
    // Suggests is excluded: deps:: tells pak to read Imports and
    // Depends only, and the design explicitly excludes Suggests
    // (see dep-mgmt.md § Server-Side Build Pipeline).
    desc := make(map[string]string)
    for _, key := range []string{
        "Imports", "Depends", "Remotes", "LinkingTo",
    } {
        if v, ok := fields[key]; ok {
            desc[key] = v
        }
    }

    m := &Manifest{
        Version:      currentVersion,
        Metadata:     meta,
        Repositories: repos,
        Description:  desc,
        Files:        files,
    }
    return m, m.Validate()
}

// parseDCF parses a Debian Control Format file (used by R DESCRIPTION
// files). Returns a map of field names to their string values.
// Continuation lines (leading whitespace) are joined to the previous field.
func parseDCF(data []byte) map[string]string {
    fields := make(map[string]string)
    var currentKey string

    for _, line := range strings.Split(string(data), "\n") {
        if len(line) == 0 {
            continue
        }
        if line[0] == ' ' || line[0] == '\t' {
            // Continuation line.
            if currentKey != "" {
                fields[currentKey] += "\n" + line
            }
            continue
        }
        if idx := strings.Index(line, ":"); idx > 0 {
            currentKey = line[:idx]
            fields[currentKey] = strings.TrimSpace(line[idx+1:])
        }
    }
    return fields
}
```

---

## Step 4: pak cache

Replace `internal/rvcache/` with `internal/pakcache/`. Same pattern:
download once, cache on the server, mount read-only into build
containers.

pak ships pre-built binaries that bundle all dependencies (pkgdepends,
curl, cli, lpSolve, etc.) into a single R package — no dependency
resolution needed to install pak itself.

```go
// internal/pakcache/pakcache.go

// EnsureInstalled downloads the pak R package to the cache directory
// if not already present. Returns the path to the cached pak package
// directory (suitable for mounting into build containers).
//
// The cached pak is a fully installed R package tree — the build
// container adds it to .libPaths() and calls pak functions directly.
func EnsureInstalled(ctx context.Context, be backend.Backend,
    image, version, cachePath string) (string, error) {

    pakDir := filepath.Join(cachePath, "pak-"+version)
    if dirExists(pakDir) {
        return pakDir, nil
    }

    // Install pak into the cache directory using a short-lived container.
    installCmd := fmt.Sprintf(
        `install.packages("pak", lib="/pak-output", repos=sprintf(`+
            `"https://r-lib.github.io/p/pak/%s/%%s/%%s/%%s", `+
            `.Platform$pkgType, R.Version()$os, R.Version()$arch))`,
        version)

    tmpDir, err := os.MkdirTemp(cachePath, ".pak-install-")
    if err != nil {
        return "", fmt.Errorf("create pak temp dir: %w", err)
    }
    defer os.RemoveAll(tmpDir)

    spec := backend.BuildSpec{
        AppID:    "_system",
        BundleID: "pak-install-" + uuid.New().String()[:8],
        Image:    image,
        Cmd:      []string{"R", "--vanilla", "-e", installCmd},
        Mounts: []backend.MountEntry{
            {Source: tmpDir, Target: "/pak-output", ReadOnly: false},
        },
        Labels: map[string]string{
            "dev.blockyard/managed": "true",
            "dev.blockyard/role":    "build",
        },
    }

    result, err := be.Build(ctx, spec)
    if err != nil {
        return "", fmt.Errorf("install pak: %w", err)
    }
    if !result.Success {
        return "", fmt.Errorf("install pak failed (exit %d): %s",
            result.ExitCode, lastLines(result.Logs, 10))
    }

    if err := os.Rename(tmpDir, pakDir); err != nil {
        return "", fmt.Errorf("move pak cache: %w", err)
    }
    return pakDir, nil
}
```

**Cache path:** `{bundle_server_path}/.pak-cache/pak-{version}/`

**Lifecycle:** cached indefinitely per version. Operators can clear the
cache by deleting the directory and restarting the server.

### Persistent download cache

pak's pkgcache stores downloaded archives keyed by URL + ETag. A
persistent directory mounted at `PKG_CACHE_DIR` across builds avoids
re-downloading packages that haven't changed upstream. This is
orthogonal to the package store (phase 2-6) which caches *installed*
packages — the download cache helps even for store misses.

**Host path:** `{bundle_server_path}/.pak-dl-cache/`

Created by the server at startup. Mounted read-write into every build
container at `/pak-cache`. Set in the build R script via
`Sys.setenv(PKG_CACHE_DIR = "/pak-cache")`.

---

## Step 5: BuildSpec extension

Add `Cmd` and `Mounts` fields to `BuildSpec` so the `Build` method
supports different build commands. Needed now (rv → pak) and reused by
the package store (phase 2-6).

```go
// internal/backend/backend.go

type BuildSpec struct {
    AppID        string
    BundleID     string
    Image        string
    RvBinaryPath string            // DEPRECATED: used only for rv compat
    BundlePath   string            // used when Mounts is empty (legacy)
    LibraryPath  string            // used when Mounts is empty (legacy)
    Labels       map[string]string
    LogWriter    func(string)
    Cmd          []string          // overrides default command when set
    Mounts       []MountEntry      // overrides default mounts when set
    Env          []string          // environment variables (KEY=VALUE)
}

type MountEntry struct {
    Source   string
    Target   string
    ReadOnly bool
}
```

### Docker backend changes

In `Build()`, use `Cmd` and `Mounts` when set:

```go
cmd := []string{"/usr/local/bin/rv", "sync"} // legacy default
if spec.Cmd != nil {
    cmd = spec.Cmd
}

var binds []string
var dockerMounts []mount.Mount
if len(spec.Mounts) > 0 {
    for _, m := range spec.Mounts {
        b, dm := d.mountCfg.TranslateMount(m)
        binds = append(binds, b...)
        dockerMounts = append(dockerMounts, dm...)
    }
} else {
    binds, dockerMounts = d.mountCfg.BuildMounts(
        spec.BundlePath, spec.LibraryPath, spec.RvBinaryPath)
}
```

### MountConfig.TranslateMount

```go
// internal/backend/docker/mounts.go

func (mc MountConfig) TranslateMount(m backend.MountEntry) (
    binds []string, mounts []mount.Mount,
) {
    switch mc.Mode {
    case MountModeVolume:
        mounts = append(mounts,
            mc.volumeMount(m.Target, m.ReadOnly, m.Source))
    case MountModeBind:
        flag := ":ro"
        if !m.ReadOnly {
            flag = ""
        }
        binds = append(binds, mc.toHostPath(m.Source)+":"+m.Target+flag)
    default: // Native
        flag := ":ro"
        if !m.ReadOnly {
            flag = ""
        }
        binds = append(binds, m.Source+":"+m.Target+flag)
    }
    return binds, mounts
}
```

---

## Step 6: Build mode detection & manifest resolution

The server resolves the bundle's dependency metadata into a manifest
before the build starts. Priority order matches the CLI (dep-mgmt.md
§ CLI Integration):

| Priority | Bundle contents | Action | Result |
|----------|----------------|--------|--------|
| 1 | `manifest.json` present | Read and validate | Manifest (pinned or unpinned) |
| 2 | `renv.lock` present | Convert to manifest (Go) | Pinned manifest |
| 3 | `DESCRIPTION` present | Convert to manifest (Go) | Unpinned manifest |
| 4 | Only scripts | Pre-process (R container) → DESCRIPTION → manifest | Unpinned manifest |

```go
// internal/bundle/buildmode.go

type BuildMode int

const (
    BuildModePinned   BuildMode = iota // manifest with packages
    BuildModeUnpinned                  // manifest with description
)

func (m BuildMode) String() string {
    switch m {
    case BuildModePinned:
        return "pinned"
    case BuildModeUnpinned:
        return "unpinned"
    default:
        return "unknown"
    }
}

// resolveManifest produces a manifest from whatever dependency metadata
// the bundle contains. Returns nil when only bare scripts are present
// (caller must run pre-processing first).
func resolveManifest(unpackedPath string) (*manifest.Manifest, error) {
    manifestPath := filepath.Join(unpackedPath, "manifest.json")
    if fileExists(manifestPath) {
        return manifest.Read(manifestPath)
    }

    meta := manifest.Metadata{
        AppMode:    detectAppMode(unpackedPath),
        Entrypoint: detectEntrypoint(unpackedPath),
    }
    files := computeFileChecksums(unpackedPath)

    renvLockPath := filepath.Join(unpackedPath, "renv.lock")
    if fileExists(renvLockPath) {
        return manifest.FromRenvLock(renvLockPath, meta, files)
    }

    descPath := filepath.Join(unpackedPath, "DESCRIPTION")
    if fileExists(descPath) {
        repos := defaultRepositories() // server default repos
        return manifest.FromDescription(descPath, meta, files, repos)
    }

    return nil, nil // bare scripts — needs pre-processing
}
```

After resolution, dispatch on manifest shape:

```go
func (m *manifest.Manifest) BuildMode() BuildMode {
    if m.IsPinned() {
        return BuildModePinned
    }
    return BuildModeUnpinned
}
```

---

## Step 7: Bare script pre-processing

When a bundle arrives without a manifest, renv.lock, or DESCRIPTION,
the server scans scripts using `pkgdepends::scan_deps()` to discover
dependencies and generates a synthetic DESCRIPTION. This is an R-side
step because `scan_deps()` requires R.

After pre-processing, the bundle is indistinguishable from a
user-supplied DESCRIPTION (including for refresh in phase 2-7).

### Pre-processing container

Mounts:

```
/app        (ro)  ← bundle (scripts only)
/pak        (ro)  ← cached pak package
/output     (rw)  ← server-managed temp directory for output
```

R script:

```r
library(pak, lib.loc = "/pak")

deps <- pkgdepends::scan_deps("/app")
pkgs <- unique(deps$package[deps$type == "prod"])

# Generate synthetic DESCRIPTION using the desc package
# (bundled with pkgdepends, which ships with pak).
dsc <- desc::desc("!new")
dsc$set(Package = "app", Version = "0.0.1")
for (p in pkgs) dsc$set_dep(p, type = "Imports")
dsc$write("/output/DESCRIPTION")
```

### Go-side orchestration

```go
// internal/bundle/preprocess.go

func preProcess(ctx context.Context, be backend.Backend,
    pakPath string, p RestoreParams) error {

    outputDir, err := os.MkdirTemp(p.BasePath, ".preprocess-")
    if err != nil {
        return fmt.Errorf("create preprocess output dir: %w", err)
    }
    defer os.RemoveAll(outputDir)

    rScript := `
        library(pak, lib.loc = "/pak")
        deps <- pkgdepends::scan_deps("/app")
        pkgs <- unique(deps$package[deps$type == "prod"])
        dsc <- desc::desc("!new")
        dsc$set(Package = "app", Version = "0.0.1")
        for (p in pkgs) dsc$set_dep(p, type = "Imports")
        dsc$write("/output/DESCRIPTION")
    `

    spec := backend.BuildSpec{
        AppID:    p.AppID,
        BundleID: p.BundleID + "-preprocess",
        Image:    p.Image,
        Cmd:      []string{"R", "--vanilla", "-e", rScript},
        Mounts: []backend.MountEntry{
            {Source: p.Paths.Unpacked, Target: "/app", ReadOnly: true},
            {Source: pakPath, Target: "/pak", ReadOnly: true},
            {Source: outputDir, Target: "/output", ReadOnly: false},
        },
        Labels: map[string]string{
            "dev.blockyard/managed": "true",
            "dev.blockyard/role":    "build",
        },
    }

    result, err := be.Build(ctx, spec)
    if err != nil {
        return fmt.Errorf("preprocess: %w", err)
    }
    if !result.Success {
        return fmt.Errorf("script scanning failed (exit %d): %s",
            result.ExitCode, lastLines(result.Logs, 10))
    }

    // Copy synthetic DESCRIPTION into the unpacked bundle dir.
    src := filepath.Join(outputDir, "DESCRIPTION")
    dst := filepath.Join(p.Paths.Unpacked, "DESCRIPTION")
    if err := copyFile(src, dst); err != nil {
        return fmt.Errorf("copy DESCRIPTION: %w", err)
    }
    return nil
}
```

After pre-processing, `resolveManifest()` finds the newly written
DESCRIPTION and produces an unpinned manifest via `FromDescription()`.
The manifest is written to the unpacked bundle dir as `manifest.json`
before the build container runs.

---

## Step 8: Build R scripts

Both pinned and unpinned modes use the same three-step pattern:
configure repositories, derive refs, run `lockfile_create()` →
`lockfile_install()`. The only difference is how refs are derived.

### Ref derivation (pinned mode)

The `record_to_ref()` helper converts an renv-style package record to
a pkgdepends ref string. This bridges two formats: renv's lockfile
records (what the manifest carries) and pkgdepends refs (what pak
consumes). See dep-mgmt.md § Ref Derivation for the full mapping.

**Provenance:** extracted from renv's internal
`renv_record_format_remote(record, pak = TRUE)` in
[`R/records.R`](https://github.com/rstudio/renv/blob/main/R/records.R).
If ref format issues arise, check renv's current implementation.

```r
record_to_ref <- function(rec) {
  # pak-installed packages carry a valid ref already.
  if (!is.null(rec$RemotePkgRef)) return(rec$RemotePkgRef)

  switch(rec$Source,
    Repository =, Bioconductor = {
      prefix <- if (rec$Source == "Bioconductor") "bioc::" else ""
      paste0(prefix, rec$Package, "@", rec$Version)
    },
    GitHub =  paste0(rec$RemoteUsername, "/", rec$RemoteRepo, "@", rec$RemoteSha),
    GitLab =  paste0("gitlab::", rec$RemoteUsername, "/", rec$RemoteRepo, "@", rec$RemoteSha),
    git    =  paste0("git::", rec$RemoteUrl),
    stop("Unsupported Source for ref derivation: ", rec$Source)
  )
}
```

### Platform URL transformation

The server transforms platform-neutral PPM URLs to platform-specific
ones so that pak receives binary packages (not source). Without this,
every package would be compiled from source — minutes per package
instead of seconds. The build container detects its own OS and distro
from `/etc/os-release` and inserts the `__linux__/{codename}/` segment.

**Provenance:** extracted from renv's internal `renv_ppm_transform()`
in [`R/ppm.R`](https://github.com/rstudio/renv/blob/main/R/ppm.R).
If PPM URL format changes, check renv's current implementation.

```r
transform_repo_url <- function(url) {
  # Only transform PPM/P3M URLs.
  if (!grepl("p3m\\.dev|packagemanager\\.posit\\.co", url)) return(url)

  # Already has a platform segment.
  if (grepl("__linux__", url)) return(url)

  # Read distro codename.
  os_release <- readLines("/etc/os-release")
  codename_line <- grep("^VERSION_CODENAME=", os_release, value = TRUE)
  codename <- sub("^VERSION_CODENAME=", "", codename_line)

  # Insert platform segment: .../cran/2026-03-18 → .../cran/__linux__/noble/2026-03-18
  sub("(/cran/|/bioc/)", paste0("\\1__linux__/", codename, "/"), url)
}
```

### Build flow

Both modes execute the same control flow. The build R script reads
`/app/manifest.json`, configures repositories, derives refs, and
runs the lockfile pipeline.

```r
library(pak, lib.loc = "/pak")
Sys.setenv(PKG_CACHE_DIR = "/pak-cache")

# ── Read manifest ────────────────────────────────────────────────
# simplifyVector = FALSE is critical: without it, jsonlite will
# simplify manifest$packages into a data frame when all records have
# identical field sets (e.g., all Source = "Repository"). vapply()
# over a data frame iterates columns, not rows — silently producing
# wrong refs. simplifyVector = FALSE keeps it as a named list of
# lists, which vapply() iterates correctly.
manifest <- jsonlite::fromJSON("/app/manifest.json",
                               simplifyVector = FALSE)

# ── Configure repositories ───────────────────────────────────────
if (length(manifest$repositories) > 0) {
  repo_urls <- setNames(
    vapply(manifest$repositories, function(r) r$URL, ""),
    vapply(manifest$repositories, function(r) r$Name, "")
  )
  # Transform platform-neutral PPM URLs to platform-specific.
  repo_urls <- vapply(repo_urls, transform_repo_url, "")
  options(repos = repo_urls)
}

# ── Derive refs ──────────────────────────────────────────────────
if (!is.null(manifest$packages)) {
  # Pinned mode: convert each package record to a pkgdepends ref.
  refs <- vapply(manifest$packages, record_to_ref, "")
} else {
  # Unpinned mode: pak reads Imports/Depends/Remotes from DESCRIPTION.
  refs <- "deps::/app"
}

# ── Phase 1: Resolve + solve (no download, no install) ──────────
pak::lockfile_create(
  refs,
  lockfile = "/build-lib/pak.lock",
  lib = "/build-lib"
)

# ── Phase 2: Download + install ─────────────────────────────────
pak::lockfile_install(
  "/build-lib/pak.lock",
  lib = "/build-lib"
)
```

In phase 2-6, store phases 2 and 4 (check store, pre-populate, ingest)
are inserted between `lockfile_create()` and `lockfile_install()`. Those
store operations are handled by the `by-builder` Go binary (introduced
in phase 2-6), not R code. The R script above is the phase 2-5 baseline.

`Suggests` are never installed. In unpinned mode, `deps::` tells pak
to read `Imports` and `Depends` only. In pinned mode, `Suggests` are
absent from the manifest because the CLI passes `renv::dependencies()`
output (which scans for actual usage) as the `packages` list to
`renv::snapshot()`.

### Container mounts

```
/app        (ro)  ← bundle (unpacked, includes manifest.json)
/pak        (ro)  ← cached pak package
/pak-cache  (rw)  ← persistent pak download cache (shared across builds)
/build-lib  (rw)  ← output library directory (per-bundle)
```

### Go-side build command

```go
// internal/bundle/restore.go

func buildCommand() []string {
    // The R script handles both pinned and unpinned modes —
    // it reads the manifest to determine which.
    rScript := `
        library(pak, lib.loc = "/pak")
        Sys.setenv(PKG_CACHE_DIR = "/pak-cache")

        # simplifyVector = FALSE: see build flow section above.
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

        pak::lockfile_create(refs,
          lockfile = "/build-lib/pak.lock", lib = "/build-lib")
        pak::lockfile_install("/build-lib/pak.lock", lib = "/build-lib")
    `
    return []string{"R", "--vanilla", "-e", rScript}
}

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
```

---

## Step 9: Restore flow

The Go-side restore orchestrates manifest resolution, optional
pre-processing, the build container, and post-build lockfile storage.

```go
// internal/bundle/restore.go

func runRestore(p RestoreParams) error {
    p.DB.UpdateBundleStatus(p.BundleID, "building")
    p.Sender.Write("restoring dependencies...")

    // 1. Ensure pak is cached.
    pakPath, err := pakcache.EnsureInstalled(
        context.Background(), p.Backend,
        p.Image, p.PakVersion, p.PakCachePath)
    if err != nil {
        return fmt.Errorf("ensure pak: %w", err)
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

    // 5. Ensure download cache dir exists.
    dlCachePath := filepath.Join(p.BasePath, ".pak-dl-cache")
    os.MkdirAll(dlCachePath, 0o755)

    // 6. Run build container.
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
        return fmt.Errorf("dependency restore failed (exit %d)", result.ExitCode)
    }

    // 7. Persist pak lockfile alongside bundle.
    lockfileSrc := filepath.Join(p.Paths.Library, "pak.lock")
    lockfileDst := filepath.Join(p.Paths.Base, "pak.lock")
    if err := copyFile(lockfileSrc, lockfileDst); err != nil {
        slog.Warn("failed to persist pak lockfile",
            "error", err, "bundle_id", p.BundleID)
        // Non-fatal — the build succeeded, lockfile is a downstream
        // optimization for store assembly (phase 2-6) and refresh (2-7).
    }

    // 8. Persist manifest alongside bundle.
    manifestDst := filepath.Join(p.Paths.Base, "manifest.json")
    if err := m.Write(manifestDst); err != nil {
        slog.Warn("failed to persist manifest",
            "error", err, "bundle_id", p.BundleID)
    }

    // 9. Activate bundle.
    if err := p.DB.ActivateBundle(p.AppID, p.BundleID); err != nil {
        return fmt.Errorf("activate bundle: %w", err)
    }

    // 10. Enforce retention.
    bundle.EnforceRetention(p.DB, p.BasePath, p.AppID, p.BundleID, p.Retention)

    return nil
}
```

### RestoreParams changes

```go
type RestoreParams struct {
    Backend      backend.Backend
    DB           *db.DB
    Tasks        *task.Store
    Sender       task.Sender
    AppID        string
    BundleID     string
    Paths        Paths
    Image        string
    PakVersion   string  // replaces RvVersion
    PakCachePath string  // replaces RvBinaryPath
    Retention    int
    BasePath     string
    AuditLog     *audit.Log
    AuditActor   string
}
```

### Post-build artifacts

After a successful build, two artifacts are stored alongside the bundle
(outside the unpacked dir, as server-managed files):

```
{bundle_server_path}/{app_id}/bundles/{bundle_id}/
├── unpacked/          # bundle contents (app.R, manifest.json, ...)
├── library/           # installed R packages (build output)
├── pak.lock           # pak lockfile — exact versions, sources, hashes
└── manifest.json      # canonical manifest (pinned or unpinned)
```

Phase 2-6 replaces this layout: `library/` is removed (workers
assemble from the store), and `store-manifest.json` (output of
`by-builder store ingest`) becomes the primary artifact driving worker
assembly, refresh comparison, and rollback. The pak lockfile is
retained as a debug/audit artifact only.

The **manifest** is preserved separately. For unpinned deploys, the
manifest retains the original `description` fields (package names with
optional version constraints), while the pak lockfile has the resolved
exact versions. The manifest drives refresh (phase 2-7); the lockfile
is informational.

---

## Step 10: Bundle validation

Relax the lockfile requirement. Only the entrypoint is mandatory:

```go
// internal/bundle/validate.go

func ValidateEntrypoint(paths Paths) error {
    for _, name := range []string{"app.R", "server.R"} {
        if fileExists(filepath.Join(paths.Unpacked, name)) {
            return nil
        }
    }
    return fmt.Errorf("missing entrypoint: app.R or server.R")
}
```

Remove `bundle.SetLibraryPath()` which modifies `rproject.toml` to
set the library path for rv. pak uses the `lib` parameter directly —
no config file modification needed.

---

## Step 11: Config changes & rv removal

### Config changes

In `internal/config/config.go`, replace `RvVersion` and `RvBinaryPath`:

```go
type DockerConfig struct {
    // ... existing fields ...
    // RvVersion    string — REMOVED
    // RvBinaryPath string — REMOVED
    PakVersion string `toml:"pak_version"` // "stable" (default), or pinned version
}
```

Default in `applyDefaults()`:

```go
if cfg.Docker.PakVersion == "" {
    cfg.Docker.PakVersion = "stable"
}
```

Config file:

```toml
[docker]
# rv_version = "v0.19.0"  — removed
pak_version = "stable"
```

Env var: `BLOCKYARD_DOCKER_PAK_VERSION`.

### rv removal

Delete or deprecate:

- `internal/rvcache/` — remove entirely (replaced by `pakcache`)
- `bundle.SetLibraryPath()` — remove (pak uses `lib` parameter)
- `BuildSpec.RvBinaryPath` — remove after migration
- `DockerConfig.RvVersion` / `DockerConfig.RvBinaryPath` — remove
- `MountConfig.BuildMounts()` — keep for now but the new
  `TranslateMount` path handles pak builds. Remove once all callers
  use `BuildSpec.Mounts`.

### Example updates

Update `examples/hello-shiny/app/`:

- Remove `rv.lock` and `rproject.toml`
- Add `DESCRIPTION`:

```
Package: hello-shiny
Title: Hello Shiny Example
Version: 0.1.0
Imports:
    shiny
```

Or for the zero-config experience, leave just `app.R` — the server
will scan it and discover `library(shiny)`.

---

## Step 12: Tests

### Unit tests

**Manifest types:**
- `TestManifestValidate_Valid` — pinned and unpinned shapes accepted.
- `TestManifestValidate_BothPackagesAndDescription` — rejected.
- `TestManifestValidate_UnknownVersion` — rejected with clear error.
- `TestManifestValidate_MissingEntrypoint` — rejected.
- `TestManifestIsPinned` — true when `packages` present, false otherwise.
- `TestManifestReadWrite` — round-trip through JSON.

**Package validation:**
- `TestPackageValidate_CRAN` — Repository source with Package/Version/Source.
- `TestPackageValidate_GitHub` — requires RemoteUsername/RemoteRepo/RemoteSha.
- `TestPackageValidate_GitHubMissingSha` — rejected with clear error.
- `TestPackageValidate_Git` — requires RemoteUrl.
- `TestPackageValidate_GitMissingUrl` — rejected.
- `TestPackageValidate_MissingSource` — rejected.
- `TestPackageValidate_UnsupportedSource` — rejected with Source name in error.

**renv.lock → manifest:**
- `TestFromRenvLock_BasicCRAN` — identity fields preserved, R.Version → platform.
- `TestFromRenvLock_GitHubPackage` — Remote* fields preserved.
- `TestFromRenvLock_BiocPackage` — Bioconductor source handled.
- `TestFromRenvLock_Repositories` — R.Repositories → manifest repositories.
- `TestFromRenvLock_V2Format` — v2 lockfile (full DESCRIPTION) works; extra fields dropped.
- `TestFromRenvLock_MissingRemoteSha` — GitHub package without RemoteSha rejected.
- `TestFromRenvLock_InvalidJSON` — error returned.

**DESCRIPTION → manifest:**
- `TestFromDescription_ImportsOnly` — basic DESCRIPTION.
- `TestFromDescription_WithRemotes` — Remotes field preserved.
- `TestFromDescription_ContinuationLines` — DCF multiline fields.
- `TestFromDescription_MissingFile` — error returned.

**DCF parser:**
- `TestParseDCF_BasicFields` — single-line fields.
- `TestParseDCF_ContinuationLines` — indented continuation.
- `TestParseDCF_EmptyLines` — skipped.

**Build mode detection:**
- `TestResolveManifest_ManifestExists` — manifest.json takes priority.
- `TestResolveManifest_RenvLock` — renv.lock converted to pinned manifest.
- `TestResolveManifest_Description` — DESCRIPTION converted to unpinned.
- `TestResolveManifest_BareScripts` — returns nil (needs pre-processing).
- `TestResolveManifest_Priority` — manifest.json wins over renv.lock.

**pakcache:**
- `TestEnsureInstalled` — first call downloads, second is cache hit.
- Cache path format verification.

**BuildSpec extension:**
- Existing Build tests pass unchanged (Cmd is nil, legacy path).
- `TestBuildWithCmd` — verify Cmd overrides default.
- `TestBuildWithMounts` — verify explicit Mounts used.
- `TestTranslateMount` — all three mount modes.

### Integration tests

- Deploy bundle with pinned manifest → pak builds correctly,
  lockfile persisted.
- Deploy bundle with renv.lock (no manifest) → converted to manifest,
  build succeeds.
- Deploy bundle with DESCRIPTION only → unpinned build, pak resolves
  and installs.
- Deploy bundle with DESCRIPTION + Remotes → GitHub package installed.
- Deploy bundle with just app.R (library(shiny)) → script scanning
  runs, DESCRIPTION generated, unpinned build succeeds.
- Deploy bundle with invalid DESCRIPTION → meaningful error.
- Deploy bundle with invalid manifest (both packages and description)
  → rejected with clear error.
- Persistent download cache: second build of different app with
  overlapping deps uses cached archives.

### E2E tests

- Update existing e2e tests to use DESCRIPTION instead of rv.lock.
- Verify the full deploy → run → access cycle works with pak.
- Verify pinned deploy with renv.lock works end-to-end.

---

## Design Decisions

1. **Manifest-driven dispatch, not file-based detection.** The old design
   detected build mode from raw file presence (pkg.lock, DESCRIPTION,
   bare scripts — three modes). The new design normalizes all inputs to
   a manifest first, then dispatches on manifest shape (pinned or
   unpinned — two modes). Bare scripts are not a separate mode; the
   server pre-processes them to a DESCRIPTION, then generates an
   unpinned manifest. This eliminates a third code path and makes the
   build uniform: every build starts from a manifest.

2. **Server-side manifest generation from renv.lock and DESCRIPTION.**
   The CLI (phase 2-9) will generate manifests before upload, but
   bundles can arrive without one (renv.lock only, DESCRIPTION only, or
   bare scripts). The server handles all cases by converting to a
   manifest before the build starts. This makes the build pipeline
   independent of the CLI and supports direct uploads during the
   transition.

3. **One R script for both modes.** The build container runs a single R
   script that reads the manifest and dispatches on `packages` vs
   `description`. The only difference is ref derivation: pinned mode
   uses `record_to_ref()`, unpinned mode uses `"deps::/app"`. Keeping
   the dispatch in R (not Go) avoids maintaining separate R script
   templates and makes the flow easier to test in isolation.

4. **Pre-processing as a separate container.** Bare-script scanning
   (`pkgdepends::scan_deps()`) runs in its own short-lived container
   before the build. The alternative — combining scan + build in one
   container — would require mounting the bundle read-write (to write
   the DESCRIPTION), which is avoided for build containers. The
   pre-processing container writes to a separate output directory; the
   server copies the DESCRIPTION into the bundle dir afterward.

5. **Lockfile_create + lockfile_install, not direct pak::pkg_install.**
   The split at the lockfile boundary is the integration point for the
   package store (phase 2-6): create the lockfile, check the store,
   pre-populate, then install. Phase 2-5 runs both steps back-to-back
   with no store in between, but the split is established now so
   phase 2-6 can insert store logic without restructuring the R script.

6. **Persistent download cache from the start.** The pak download cache
   (`PKG_CACHE_DIR`) avoids re-downloading archives across builds. It's
   orthogonal to the package store (phase 2-6) and costs one mount
   entry. Including it in phase 2-5 means builds are faster immediately,
   before the store exists.

7. **Post-build lockfile + manifest storage.** Both artifacts are
   persisted alongside the bundle but serve different purposes. The pak
   lockfile has exact resolved versions and is retained as a debug/audit
   artifact. Worker library assembly (phase 2-6) and refresh comparison
   (phase 2-7) are driven by `store-manifest.json` (output of
   `by-builder store ingest`), not the lockfile. The manifest retains
   the original dependency specification (unpinned: description fields;
   pinned: package records) and drives refresh (phase 2-7). The lockfile
   is a non-fatal artifact in phase 2-5 — the build succeeds without
   it.
