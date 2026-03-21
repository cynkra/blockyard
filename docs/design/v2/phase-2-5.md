# Phase 2-5: Build Pipeline — rv → pak

Replace rv with pak as the build-time dependency manager. Make lockfiles
optional. Support three build modes depending on what the bundle ships:
lockfile (exact reproducibility), DESCRIPTION (standard R package deps),
or bare scripts (zero-config deploy via script scanning). This simplifies
the deployment workflow — users no longer need rv or any special tooling
to prepare bundles — and aligns blockyard with the standard R ecosystem.

## Deliverables

1. **pak cache** — download and cache pak's pre-built bundle on the
   server, replacing the rv binary cache.
2. **Build mode detection** — determine how to resolve dependencies
   based on bundle contents (lockfile → DESCRIPTION → script scan).
3. **Build container command** — R script that loads pak and runs the
   appropriate resolution + install strategy.
4. **BuildSpec extension** — add `Cmd` and `Mounts` fields so the
   `Build` method supports flexible build commands beyond the current
   hardcoded `rv sync`.
5. **Config changes** — replace `rv_version` with `pak_version`.
6. **Bundle validation** — relax the lockfile requirement; accept
   bundles with only `app.R`.
7. **Migration** — update examples, documentation, tests.

---

## Step 1: pak cache

Replace `internal/rvcache/` with `internal/pakcache/`. Same pattern:
download once, cache on the server, mount read-only into build
containers.

pak ships pre-built binaries that bundle all dependencies (pkgdepends,
curl, cli, lpSolve, etc.) into a single R package — no dependency
resolution needed to install pak itself.

```go
// internal/pakcache/pakcache.go

const (
    // Pre-built binary repo. Platform-specific URL is constructed at runtime.
    pakRepoBase = "https://r-lib.github.io/p/pak"
)

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
    // The container runs R, installs pak from the pre-built binary repo,
    // and writes the installed package tree to the mounted output dir.
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

    // Atomically move to final location.
    if err := os.Rename(tmpDir, pakDir); err != nil {
        return "", fmt.Errorf("move pak cache: %w", err)
    }
    return pakDir, nil
}
```

**Cache path:** `{bundle_server_path}/.pak-cache/pak-{version}/`

**Lifecycle:** cached indefinitely per version. Operators can clear the
cache by deleting the directory and restarting the server.

### Config changes

In `internal/config/config.go`, replace `RvVersion` and `RvBinaryPath`:

```go
type DockerConfig struct {
    // ... existing fields ...
    // RvVersion  string — REMOVED
    // RvBinaryPath string — REMOVED
    PakVersion string `toml:"pak_version"` // "stable" (default), "devel", or pinned
}
```

Default in `applyDefaults()`:

```go
if cfg.Docker.PakVersion == "" {
    cfg.Docker.PakVersion = "stable"
}
```

Config file change:

```toml
[docker]
# rv_version = "v0.19.0"  — removed
pak_version = "stable"     # "stable", "devel", or specific version
```

Env var: `BLOCKYARD_DOCKER_PAK_VERSION`.

---

## Step 2: Build mode detection

When a bundle is uploaded, the server inspects its contents to determine
the dependency resolution strategy. Checked in priority order:

| Priority | File present | Strategy | pak function |
|----------|-------------|----------|-------------|
| 1 | `pkg.lock` | Lockfile restore | `pak::lockfile_install()` |
| 2 | `DESCRIPTION` | Package deps | `pak::local_install_deps()` |
| 3 | `app.R` only | Script scanning | `pak::pkg_install(pak::scan_deps()$package)` |

```go
// internal/bundle/buildmode.go

type BuildMode int

const (
    BuildModeLockfile    BuildMode = iota // pkg.lock present
    BuildModeDescription                  // DESCRIPTION present
    BuildModeScan                         // bare scripts only
)

func DetectBuildMode(unpackedPath string) BuildMode {
    if fileExists(filepath.Join(unpackedPath, "pkg.lock")) {
        return BuildModeLockfile
    }
    if fileExists(filepath.Join(unpackedPath, "DESCRIPTION")) {
        return BuildModeDescription
    }
    return BuildModeScan
}
```

**Lockfile format:** pak's native `pkg.lock`, created by
`pak::lockfile_create()`. Users run this locally for reproducible
deploys. renv.lock is not supported — pak lockfiles are the single
format.

**DESCRIPTION mode:** standard R package DESCRIPTION file with
`Imports`, `Depends`, and optionally `Remotes` for GitHub/git deps.
This is how most R packages declare dependencies and is familiar to
all R developers.

**Scan mode:** pak scans all `.R` files for `library()`, `require()`,
`::` calls and resolves + installs the discovered packages. Zero
config — just upload app.R and deploy.

### Bundle validation changes

In `bundle.ValidateEntrypoint()`, remove the rv.lock requirement.
Only `app.R` is mandatory:

```go
func ValidateEntrypoint(paths Paths) error {
    entrypoint := filepath.Join(paths.Unpacked, "app.R")
    if _, err := os.Stat(entrypoint); err != nil {
        return fmt.Errorf("missing entrypoint: app.R")
    }
    return nil
}
```

### SetLibraryPath removal

Remove `bundle.SetLibraryPath()` which modifies `rproject.toml` to
set the library path for rv. pak uses the `lib` parameter directly —
no config file modification needed.

---

## Step 3: BuildSpec extension

Add `Cmd` and `Mounts` fields to `BuildSpec` so the `Build` method
supports different build commands. This is needed now (rv → pak) and
will be reused by the package store (phase 2-6).

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

## Step 4: Build container command

The restore flow in `internal/bundle/restore.go` changes from running
`rv sync` to running an R script that uses pak.

### Build command per mode

```go
// internal/bundle/restore.go

func buildCommand(mode BuildMode) []string {
    var rScript string
    switch mode {
    case BuildModeLockfile:
        rScript = `
            library(pak, lib.loc = "/pak")
            lockfile_install(
                lockfile = "/app/pkg.lock",
                lib = "/build-lib",
                update = TRUE
            )`
    case BuildModeDescription:
        rScript = `
            library(pak, lib.loc = "/pak")
            local_install_deps(
                root = "/app",
                lib = "/build-lib",
                upgrade = FALSE,
                ask = FALSE
            )`
    case BuildModeScan:
        rScript = `
            library(pak, lib.loc = "/pak")
            deps <- scan_deps("/app")
            pkgs <- unique(deps$package)
            pkgs <- setdiff(pkgs, rownames(installed.packages(
                lib.loc = .Library)))
            if (length(pkgs) > 0) {
                pkg_install(pkgs, lib = "/build-lib", ask = FALSE)
            }`
    }
    return []string{"R", "--vanilla", "-e", rScript}
}
```

### Mount setup

```go
func buildMounts(pakCachePath, bundlePath, libraryPath string) []backend.MountEntry {
    return []backend.MountEntry{
        {Source: bundlePath, Target: "/app", ReadOnly: true},
        {Source: libraryPath, Target: "/build-lib", ReadOnly: false},
        {Source: pakCachePath, Target: "/pak", ReadOnly: true},
    }
}
```

pak is mounted read-only at `/pak` and loaded via
`library(pak, lib.loc = "/pak")`. The app bundle is read-only at
`/app`. The output library is writable at `/build-lib`.

### Restore flow changes

In `runRestore()`:

```go
func runRestore(p RestoreParams) error {
    // 1. Update bundle status to "building".
    p.DB.UpdateBundleStatus(p.BundleID, "building")
    p.Sender.Write("restoring dependencies...")

    // 2. Ensure pak is cached.
    pakPath, err := pakcache.EnsureInstalled(
        context.Background(), p.Backend,
        p.Image, p.PakVersion, p.PakCachePath)
    if err != nil {
        return fmt.Errorf("ensure pak: %w", err)
    }

    // 3. Detect build mode.
    mode := DetectBuildMode(p.Paths.Unpacked)
    p.Sender.Write(fmt.Sprintf("build mode: %s", mode))

    // 4. Build.
    spec := backend.BuildSpec{
        AppID:    p.AppID,
        BundleID: p.BundleID,
        Image:    p.Image,
        Cmd:      buildCommand(mode),
        Mounts:   buildMounts(pakPath, p.Paths.Unpacked, p.Paths.Library),
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

    // 5. Activate bundle.
    if err := p.DB.ActivateBundle(p.AppID, p.BundleID); err != nil {
        return fmt.Errorf("activate bundle: %w", err)
    }

    // 6. Enforce retention.
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

---

## Step 5: Remove rv

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

Or for the zero-config experience, leave just `app.R` — pak will
scan it and discover `library(shiny)`.

---

## Step 6: Tests

### Unit tests

**pakcache:**
- `TestEnsureInstalled` — first call downloads, second is cache hit.
- Cache path format verification.

**Build mode detection:**
- `TestDetectBuildMode_Lockfile` — pkg.lock present → lockfile mode.
- `TestDetectBuildMode_Description` — DESCRIPTION present → description mode.
- `TestDetectBuildMode_Scan` — only app.R → scan mode.
- Priority: pkg.lock wins over DESCRIPTION.

**Build command:**
- `TestBuildCommand_Lockfile` — verify R script uses `lockfile_install`.
- `TestBuildCommand_Description` — verify R script uses `local_install_deps`.
- `TestBuildCommand_Scan` — verify R script uses `scan_deps` + `pkg_install`.

**BuildSpec extension:**
- Existing Build tests pass unchanged (Cmd is nil, legacy path).
- `TestBuildWithCmd` — verify Cmd overrides default.
- `TestBuildWithMounts` — verify explicit Mounts used.
- `TestTranslateMount` — all three mount modes.

### Integration tests

- Deploy bundle with pkg.lock → restore succeeds, packages installed.
- Deploy bundle with DESCRIPTION → restore succeeds, packages installed.
- Deploy bundle with just app.R (library(shiny)) → restore succeeds,
  shiny installed.
- Deploy bundle with DESCRIPTION + Remotes field → GitHub package
  installed.
- Deploy bundle with invalid DESCRIPTION → meaningful error.

### E2E tests

- Update existing e2e tests to use DESCRIPTION instead of rv.lock.
- Verify the full deploy → run → access cycle works with pak.

---

## Design Decisions

1. **pak over renv.** pak has a proper constraint solver (via
   pkgdepends/lpSolve), broader script scanning, lockfile support,
   and bundles all its dependencies into a single self-contained
   package. renv lacks a solver and is primarily a project isolation
   tool, not a dependency resolver.

2. **pak's pre-built bundle, not CRAN pak.** The pre-built binary from
   `r-lib.github.io/p/pak/` vendors all 16 dependencies (pkgdepends,
   curl, cli, etc.) into one package. Install is a single download +
   unpack — same pattern as rv binary caching. The CRAN version of pak
   (installed by users into their apps) is a separate concern.

3. **Three build modes with clear priority.** Lockfile wins (exact
   repro), then DESCRIPTION (standard R), then scan (zero config).
   This covers the spectrum from CI/CD reproducibility to quick
   prototyping. Users opt into reproducibility by adding a lockfile;
   they don't need to opt out of it.

4. **No rproject.toml.** This was rv-specific. pak reads standard R
   files (DESCRIPTION, pkg.lock). No proprietary config format.

5. **pak lockfile format (pkg.lock).** pak's native format, not
   renv.lock. One tool, one lockfile format. Users generate it with
   `pak::lockfile_create()`.

6. **Server-side pak cache, not in the pkg-store.** pak-the-build-tool
   is internal infrastructure, cached alongside server binaries. If a
   user's app depends on pak as an R package, that's a separate CRAN
   install that goes through the normal dependency path.
