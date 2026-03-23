# Dependency Management

Unified design for how blockyard discovers, resolves, caches, and
serves R package dependencies across the full lifecycle: client-side
CLI, server-side build, runtime assembly, and live package requests.

This document supersedes the dependency-specific sections of
phase 2-5 (build pipeline) and phase 2-6 (package store) and adds
the manifest format, CLI integration, and runtime update mechanics.
The implementation details in those phase docs (Go structs, mount
logic, API endpoints) remain authoritative for their scope — this
document provides the architectural overview that ties them together.

## Prior Art

| Platform | Client tool | Lockfile format | Server-side resolution | Notes |
|---|---|---|---|---|
| Posit Connect | rsconnect R pkg | `manifest.json` (embeds full DESCRIPTION per package) | No — client resolves everything | Uses `renv::dependencies()` + `renv::snapshot()` internally, translates to manifest |
| Posit Connect Cloud | rsconnect / Publisher | `manifest.json` committed to git | No | Same format, git-backed deploys |
| Posit Publisher | VS Code extension (Go backend) | `renv.lock` required, translated to `manifest.json` | No | Can build manifest from lockfile alone (no installed packages needed) |
| Ricochet | Rust CLI | `renv.lock` shipped as-is | Yes — `renv::restore()` in build container | No manifest; lockfile is the wire format. No fallback without lockfile |
| blockyard v1 | N/A (direct upload) | `rv.lock` (TOML) | No — `rv sync` from lockfile | rv-specific format, lockfile required |

**Key observation:** no existing platform supports server-side
dependency *discovery* (upload code without any dependency metadata
and let the server figure it out). blockyard's scan mode is novel.

---

## Architecture Overview

```
 CLI (user's machine)                    Server
 ────────────────────                    ──────

 ┌─────────────┐     upload bundle
 │ renv.lock   │──┐  + manifest.json     ┌──────────────────┐
 │ (optional)  │  │                       │ Build pipeline   │
 └─────────────┘  ├─────────────────────▸ │                  │
 ┌─────────────┐  │                       │ Has manifest?    │
 │ DESCRIPTION │──┤                       │  yes → install   │
 │ (optional)  │  │                       │  no  → pak scan  │
 └─────────────┘  │                       │        ↓         │
 ┌─────────────┐  │                       │  generate manifest│
 │ app.R only  │──┘                       │        ↓         │
 └─────────────┘                          │  install deps    │
                                          │        ↓         │
                                          │  ingest → store  │
                                          └──────────────────┘
                                                   │
                                                   ▼
                                          ┌──────────────────┐
                                          │ Package store    │
                                          │ (renv-style hash)│
                                          └──────────────────┘
                                                   │
                                            hard links
                                                   ▼
                                          ┌──────────────────┐
                                          │ Worker container  │
                                          │ /app-lib (ro)    │
                                          │ /extra-lib (rw)  │
                                          └──────────────────┘
```

Three deployment paths, in order of reproducibility:

1. **Manifest present** — CLI produced a `manifest.json` from
   `renv.lock` or user-provided metadata. Server installs exactly
   what the manifest specifies. Most reproducible.
2. **DESCRIPTION present** — standard R package metadata. Server
   runs `pak::local_install_deps()`. Reproducible up to repo
   snapshot date.
3. **Bare scripts** — server runs `pak::scan_deps()` to discover
   `library()` / `::` calls, then `pak::pkg_install()`. Least
   reproducible but zero-config.

---

## Manifest Format

The blockyard manifest follows the Connect `manifest.json`
structure as closely as practical. Connect's format is the de facto
standard — rsconnect, Publisher, and Connect Cloud all produce or
consume it. We adopt its structure and field names, omitting fields
that are not operationally useful (the full embedded DESCRIPTION per
package) and extending where needed.

### Schema

```json
{
  "version": 1,
  "locale": "en_US",
  "platform": "4.4.2",
  "metadata": {
    "appmode": "shiny",
    "entrypoint": "app.R"
  },
  "repositories": [
    { "Name": "CRAN", "URL": "https://p3m.dev/cran/__linux__/noble/2026-03-18" }
  ],
  "packages": {
    "shiny": {
      "Source": "CRAN",
      "Repository": "https://p3m.dev/cran/__linux__/noble/2026-03-18",
      "Version": "1.9.1",
      "Hash": "a1b2c3d4e5f6...",
      "Requirements": ["httpuv", "mime", "jsonlite", "htmltools", "R6"],
      "description": {
        "Package": "shiny",
        "Version": "1.9.1",
        "Depends": "R (>= 3.0.2)",
        "Imports": "utils, grDevices, httpuv (>= 1.5.2), mime (>= 0.3), jsonlite (>= 0.9.16), htmltools (>= 0.5.4), R6 (>= 2.0), later (>= 1.0.0), promises (>= 1.1.0), rlang (>= 0.4.10), fastmap (>= 1.1.1), bslib (>= 0.6.0), cachem, lifecycle (>= 0.2.0)",
        "LinkingTo": null,
        "NeedsCompilation": "no",
        "Repository": "CRAN"
      }
    },
    "myghpkg": {
      "Source": "GitHub",
      "Version": "0.3.1",
      "Hash": "d4e5f6a7b8c9...",
      "Requirements": ["rlang", "cli"],
      "GithubUsername": "owner",
      "GithubRepo": "myghpkg",
      "GithubRef": "main",
      "GithubSha1": "a1b2c3d4e5f6789...",
      "description": {
        "Package": "myghpkg",
        "Version": "0.3.1",
        "Imports": "rlang, cli",
        "LinkingTo": null,
        "NeedsCompilation": "no",
        "RemoteType": "github",
        "RemoteSha": "a1b2c3d4e5f6789..."
      }
    }
  },
  "files": {
    "app.R": { "checksum": "abc123..." }
  }
}
```

### Relationship to Connect's Format

We retain Connect's field names and casing (`Source`, `Repository`,
`Version`, `GithubUsername`, `GithubRepo`, `GithubSha1`, etc.) for
maximum familiarity to R developers who have worked with Connect
deployments.

**What we keep from Connect:**
- `version`, `locale`, `platform` top-level fields
- `metadata.appmode`, `metadata.entrypoint`
- `packages` map keyed by package name
- `Source`, `Repository`, `Version`, `Github*` fields per package
- `files` map with checksums

**What we add:**
- `repositories` — structured array of `{Name, URL}`. Connect
  records repo URLs per-package in the `Repository` field and in
  the embedded DESCRIPTION; we also provide them as a top-level
  array for easy server-side repo configuration.
- `Hash` — renv-style MD5 hash of the package DESCRIPTION (see
  Cache Key Design below). Connect doesn't include this.
- `Requirements` — flat list of dependency package names (from
  renv.lock). Connect embeds full DESCRIPTION instead.

**What we trim from Connect:**
- The full embedded `description` object per package. Connect
  embeds every DESCRIPTION field (Title, Authors, License,
  BugReports, URL, Encoding, etc.). We include only the
  operationally relevant subset: `Package`, `Version`, `Depends`,
  `Imports`, `LinkingTo`, `NeedsCompilation`, `Repository`, and
  `Remote*` fields. This reduces manifest size by ~5-10x.

### Design Rationale

**Why not ship `renv.lock` directly (like Ricochet)?**
renv.lock lacks the `repositories` array, `appmode` metadata, and
file checksums. It also doesn't map cleanly to the Connect ecosystem
that R developers already know. A manifest gives us a single format
that works for all three build modes (the server generates one for
DESCRIPTION and scan modes too).

**Why lean on Connect's field names?**
Any R developer who has deployed to Connect recognizes `Source`,
`Repository`, `GithubSha1`, etc. Using the same vocabulary reduces
cognitive load and means existing tooling (rsconnect, Publisher)
could potentially produce compatible manifests with minimal
adaptation.

**Why include `Hash`?**
The renv-style hash is our package store cache key (see below).
Including it in the manifest means the server can check for cache
hits without reading installed DESCRIPTION files.

---

## CLI Integration (Phase 2-8)

The CLI (`cmd/by/`) produces the manifest from the user's local
environment.

### Manifest Generation Flow

```
by deploy ./myapp/

  1. Check for existing manifest.json
     └─ found → use it (skip to upload)

  2. Check for renv.lock
     └─ found → parse it, build manifest (no R needed)

  3. R available? renv available?
     └─ yes → renv::dependencies() + renv::snapshot()
              → parse generated renv.lock → build manifest
              → clean up renv artifacts
     └─ no  → warn: "dependencies will be resolved server-side"
              → upload without manifest (falls through to
                DESCRIPTION or scan mode on the server)
```

### renv.lock → Manifest Translation

The CLI reads `renv.lock` (plain JSON) and translates each package
record. This is a pure data transformation in Go — no R subprocess
needed.

```
renv.lock                          manifest.json
─────────                          ─────────────
R.Version               →          platform
R.Repositories          →          repositories
Packages.*.Package      →          packages key
Packages.*.Version      →          packages.*.Version
Packages.*.Source       →          packages.*.Source
Packages.*.Repository   →          packages.*.Repository (resolved to URL via R.Repositories)
Packages.*.Hash         →          packages.*.Hash
Packages.*.Requirements →          packages.*.Requirements
Packages.*.RemoteType   →          (used to derive Source when present)
Packages.*.RemoteUsername →        packages.*.GithubUsername
Packages.*.RemoteRepo   →          packages.*.GithubRepo
Packages.*.RemoteRef    →          packages.*.GithubRef
Packages.*.RemoteSha    →          packages.*.GithubSha1
```

For the trimmed `description` object, the CLI reads the installed
package's DESCRIPTION if available (to get `Depends`, `Imports`,
`LinkingTo`, `NeedsCompilation`). When packages are not installed
locally (renv.lock from another machine), the `description` is
omitted — the server can reconstruct it after installation.

### renv Availability

renv is not part of base R. The CLI handles this gracefully:

| State | Behavior |
|---|---|
| `renv.lock` exists | Parse directly in Go. No R needed. |
| No lockfile, R + renv available | Shell out: `Rscript -e 'renv::dependencies() + renv::snapshot()'`. Parse result. Clean up. |
| No lockfile, R available, renv missing | Attempt `install.packages("renv")`. If that fails, warn and degrade to server-side resolution. |
| No lockfile, no R | Warn and degrade. Upload without manifest. |

Degradation is always explicit: the CLI tells the user what's
happening and why.

### renv Invocation

Following rsconnect's pattern (`snapshotRenvDependencies()`):

```r
options(renv.consent = TRUE)
deps <- renv::dependencies(".", quiet = TRUE, progress = FALSE)
renv::snapshot(".", packages = deps$Package, prompt = FALSE)
```

The CLI runs this via `Rscript -e`, reads the resulting `renv.lock`
from disk, then cleans up (`renv.lock`, `renv/` directory) unless
they pre-existed.

---

## Server-Side Build Pipeline

### Build Mode Detection

Checked in priority order:

| Priority | Condition | Strategy |
|---|---|---|
| 1 | `manifest.json` present | Install from manifest |
| 2 | `DESCRIPTION` present | `pak::local_install_deps()` then generate manifest |
| 3 | Only scripts | `pak::scan_deps()` + `pak::pkg_install()` then generate manifest |

In modes 2 and 3 the server generates a `manifest.json` after
the build completes and stores it alongside the bundle. This
means every successful build produces a manifest — useful for
auditing and for re-building the same bundle without re-resolving.

### Manifest-Based Install

When a manifest is present, the server installs packages using pak
with explicit version pins:

```r
library(pak, lib.loc = "/pak")

manifest <- jsonlite::fromJSON("/app/manifest.json")

# Set repositories from manifest
repos <- setNames(manifest$repositories$URL, manifest$repositories$Name)
options(repos = repos)

# Build ref list with version pins
refs <- vapply(names(manifest$packages), function(name) {
  pkg <- manifest$packages[[name]]
  if (identical(pkg$Source, "GitHub")) {
    sprintf("%s/%s@%s", pkg$GithubUsername, pkg$GithubRepo, pkg$GithubSha1)
  } else {
    # pak ref format: package@version
    sprintf("%s@%s", name, pkg$Version)
  }
}, character(1))

pak::pkg_install(refs, lib = "/build-lib", upgrade = FALSE, ask = FALSE)
```

The manifest's `repositories` array sets the exact repo URLs
(including dated PPM snapshots), so `pak::pkg_install("shiny@1.9.1")`
resolves unambiguously within that repo snapshot.

### How pak Works (Relevant Internals)

pak never calls `install.packages()`. It has its own pipeline:

1. **Resolve** — for each ref, query CRAN/Bioc metadata or GitHub
   API. Also scan the target library (`make_installed_cache()`) to
   discover already-installed packages.
2. **Solve** — formulate an Integer Linear Programming problem
   (via lpSolve). The default "lazy" policy assigns cost 0 to
   installed packages, 1 to binary downloads, 5 to source builds.
   Output: an install plan data frame with `lib_status` per package
   (`current` = already installed, `new`, `update`, etc.).
3. **Download** — fetch package archives via pkgcache (a
   content-addressable download cache keyed by URL + ETag).
4. **Install** — for binary packages: extract archive +
   `file.rename()` into the library. For source packages:
   `R CMD INSTALL --build` via pkgbuild, then extract the
   resulting binary. `install_package_plan()` skips any package
   with `lib_status == "current"`.

`install_package_plan()` is exported and accepts a plan data frame
directly. This means we can intercept between steps 3 and 4: get
the plan, hardlink cache hits from our store into the library, mark
them as `"current"`, and call `install_package_plan()` to install
only the remainder.

### Store-Aware Build Flow

Every build mode uses the same pattern: resolve what's needed, check
our store for cache hits, install only what's missing, then ingest
newly installed packages back into the store.

**Container mounts:**

```
/app        (ro)  ← bundle
/build-lib  (rw)  ← output library (initially empty)
/pak        (ro)  ← cached pak package
/pak-cache  (rw)  ← persistent pak download cache (shared across builds)
/store      (ro)  ← package store (multi-version, shared across builds)
```

The persistent download cache (`/pak-cache`) is set via
`Sys.setenv(PKG_CACHE_DIR = "/pak-cache")`. This avoids
re-downloading archives across builds — pak's pkgcache checks
ETags and serves from cache when fresh.

**Manifest mode (single solve):**

```r
library(pak, lib.loc = "/pak")
library(pkgdepends, lib.loc = "/pak")
Sys.setenv(PKG_CACHE_DIR = "/pak-cache")

manifest <- jsonlite::fromJSON("/app/manifest.json")

# Build refs from manifest
repos <- setNames(manifest$repositories$URL, manifest$repositories$Name)
options(repos = repos)

refs <- vapply(names(manifest$packages), function(name) {
  pkg <- manifest$packages[[name]]
  if (identical(pkg$Source, "GitHub")) {
    sprintf("%s/%s@%s", pkg$GithubUsername, pkg$GithubRepo, pkg$GithubSha1)
  } else {
    sprintf("%s@%s", name, pkg$Version)
  }
}, character(1))

# Single solve
proposal <- new_pkg_installation_proposal(
  refs, config = list(library = "/build-lib")
)
proposal$resolve()
proposal$solve()
proposal$download()
plan <- proposal$get_install_plan()

# Pre-populate cache hits from store
for (i in seq_len(nrow(plan))) {
  if (plan$lib_status[i] %in% c("new", "update")) {
    hash <- manifest$packages[[plan$package[i]]]$Hash
    store_path <- file.path("/store", plan$package[i], hash)
    if (dir.exists(store_path)) {
      link_package(store_path, file.path("/build-lib", plan$package[i]))
      plan$lib_status[i] <- "current"
    }
  }
}

# Install only cache misses
install_package_plan(plan, lib = "/build-lib", num_workers = 4)
```

For full cache hits (all packages in the store), the install step
is a no-op — the build completes in seconds.

**DESCRIPTION and scan modes** follow the same pattern. The only
difference is how `refs` are constructed:

- DESCRIPTION: `refs <- "deps::/app"` — pkgdepends reads
  `Imports`, `Depends`, and `Remotes` from the DESCRIPTION file.
- Scan: `refs <- unique(pak::scan_deps("/app")$package)` — pak
  scans `.R` files for `library()`, `require()`, and `::` calls.

For these modes there is no manifest with pre-computed hashes, so
the store lookup uses the resolved `package` + `version` from the
plan. The hash is computed from the installed DESCRIPTION after the
build completes (see Store Population below).

**Post-build: store ingestion and manifest generation.** After a
successful build, the server:

1. Walks `/build-lib` and computes the renv-style hash for each
   installed package.
2. For packages not already in the store, copies the installed
   package tree into the store at `{package}/{hash}/`.
3. For DESCRIPTION and scan modes, generates a `manifest.json`
   from the install plan and stores it alongside the bundle.

---

## Package Store and Cache Key Design

### The ABI Problem

R packages with compiled code (`NeedsCompilation: yes`) that use
`LinkingTo` compile against header files from the linked package.
The resulting `.so` contains hardcoded assumptions about struct
layouts, function signatures, and ABI conventions from the linked
package's headers *at build time*.

If the linked package later changes those (e.g., Rcpp changes a
struct layout), the dependent package can crash, produce wrong
results, or fail to load. This is not theoretical — the
`Rcpp_precious_remove` incident (2021) broke sf, lme4, and
hundreds of other packages.

**How the ecosystem handles this:**

- **CRAN:** does NOT automatically rebuild reverse-LinkingTo
  dependencies. Manual/ad-hoc only.
- **PPM/P3M:** rebuilds the entire reverse-LinkingTo chain before
  publishing a snapshot. All binaries within a dated snapshot are
  guaranteed mutually compatible.
- **renv:** cache key is an MD5 of the DESCRIPTION file. Same
  package version compiled against different LinkingTo versions
  gets the **same cache key**. This is a
  [known open issue](https://github.com/rstudio/renv/issues/884).
- **R itself:** no mechanism to detect or trigger rebuilds. No
  build-time metadata recorded.

### Our Approach: renv-Style Hash + Snapshot Coherence

We adopt renv's DESCRIPTION-based hash as the cache key. This
aligns with ecosystem conventions and means the hash can be
computed from the manifest (which includes `Hash`) without needing
the installed package tree.

**Store key format:**

```
{package}/{hash}/
```

Following renv's cache layout: the hash is an MD5 of selected
DESCRIPTION fields (`Package`, `Version`, `Title`, `Author`,
`Maintainer`, `Description`, `Depends`, `Imports`, `Suggests`,
`LinkingTo`, plus `Remote*` fields for non-CRAN packages).

Examples:
```
shiny/a1b2c3d4e5f6.../
Rcpp/f8e9d0c1b2a3.../
blockr/d4e5f6a7b8c9.../
```

**ABI safety relies on PPM snapshot coherence.** Within a single
PPM dated snapshot, all binaries are built against each other —
the library is coherent. Builds that install from a single snapshot
produce a coherent store population. This is the primary safety
mechanism.

The one case where ABI coherence can break is **GitHub dev-installs
of packages with `LinkingTo`**. A GitHub package compiled from
source uses headers from whatever version of the linked package is
currently installed. If that version differs from what other cached
packages were compiled against, ABI mismatch is possible.

This is a narrow edge case (GitHub install + compiled code +
`LinkingTo` a package that changed ABI between the installed version
and the version in the store). The server detects and aborts:

1. At install time, read the GitHub package's `LinkingTo` field.
2. For each linked package, check whether it is already loaded in
   the session at a version different from what is installed in
   the build library.
3. If there is a mismatch, abort with a clear error: "package X
   links against Y, but Y is loaded at a different version.
   Rebuild required."

This avoids silent ABI corruption. The user's recourse is to
rebuild the app with updated dependencies.

### Store Layout

The store holds multiple versions of the same package, keyed by
hash. Each entry is a fully installed R package tree:

```
.pkg-store/
├── shiny/
│   ├── a1b2c3d4.../shiny/    ← v1.9.1 from CRAN
│   └── e5f6a7b8.../shiny/    ← v1.8.0 from CRAN (different hash)
├── ggplot2/
│   └── c9d0e1f2.../ggplot2/  ← v3.5.0
├── blockr/
│   ├── f3a4b5c6.../blockr/   ← v0.2.0 from CRAN
│   └── d7e8f9a0.../blockr/   ← v0.2.1-dev from GitHub (different hash)
└── ...
```

An R library is flat — `lib/shiny/` can hold exactly one version.
The bridge between the multi-version store and R's single-version
library is a **view**: a flat directory assembled per-build (or
per-worker) by hard-linking the correct version of each package
from the store.

```
/build-lib/                      (assembled view)
├── shiny/    → hardlink from .pkg-store/shiny/a1b2c3d4.../shiny/
├── ggplot2/  → hardlink from .pkg-store/ggplot2/c9d0e1f2.../ggplot2/
└── blockr/   → hardlink from .pkg-store/blockr/f3a4b5c6.../blockr/
```

Version selection depends on context:

- **Manifest mode:** the manifest's `Hash` field maps directly to
  a store entry. Exact match.
- **DESCRIPTION / scan modes:** the install plan's resolved
  `package` + `version` is matched against the store. The hash is
  computed from the installed DESCRIPTION after the build, then
  used for future lookups.
- **Runtime views** (worker `/extra-lib`): the manifest of the
  running bundle specifies which hashes to link.

Hard links (not symlinks) are used so the store does not need to
be mounted into worker containers at runtime. The view is
self-contained.

### Store Population

Packages enter the store after successful builds. The server:

1. Walks the build library (`/build-lib`).
2. For each installed package, reads its DESCRIPTION and computes
   the renv-style MD5 hash.
3. If `{package}/{hash}/` does not exist in the store, copies the
   installed package tree into the store. If it already exists
   (another build already installed the same version), skips it.

The store is append-only — packages are never modified or deleted
after insertion. Eviction (LRU, size limits) is a future concern.

---

## Runtime Package Assembly

### Worker Startup

When a worker container starts:

1. The bundle's own library is mounted read-only at `/app-lib`.
2. A per-worker view directory is created and mounted at
   `/extra-lib` (initially empty).
3. `R_LIBS=/extra-lib:/app-lib` — extra-lib shadows app-lib.

If the bundle's manifest lists packages already in the store, they
are pre-linked into `/extra-lib` at spawn time. This covers the
common case where a blockr board needs packages beyond the app's
own dependencies.

### Live Package Requests

A running worker can request additional packages via the runtime
assembly API (`POST /api/v1/packages`). Three outcomes:

**1. Cache hit, no conflict:** package is in the store and not yet
loaded in the R session. Hard-link into `/extra-lib`. R finds it on
next `library()` call. Instant.

**2. Cache miss:** package is not in the store. For v2, the API
returns the package in a `missing` list and the app handles it
(degrade gracefully, show a message, or request a redeploy). Future
options include in-container install or background build jobs.

**3. Version conflict:** the requested package (or a dependency) is
already loaded in the R session at a different version. R cannot
unload and reload a package at a different version in the same
session. This requires a container transfer.

### Container Transfer (Version Conflict Case)

When a package update requires a new container:

```
 Worker A (old)              Server              Worker B (new)
 ──────────────              ──────              ──────────────
 1. Request pkg update
        ────────────▸ 2. Detect conflict
                      3. Spawn Worker B with
                         updated library view
                                   ────────────▸ 4. Worker B ready
                      5. Signal Worker A:
                         "flush board state"
 6. blockr serializes
    board to JSON file
    on shared volume
        ────────────▸ 7. Signal Worker B:
                         "restore board"
                                   ────────────▸ 8. blockr reads JSON,
                                                    restores board
                      9. Reroute traffic
                         A → B
                     10. Drain & stop A
```

**We do NOT serialize the R session.** blockr has built-in board
serialization (to/from JSON). The transfer is:

1. Signal Worker A to flush board state to a JSON file on a shared
   volume.
2. Start Worker B with the updated library and mount the same
   shared volume.
3. Worker B reads the JSON file and restores the board.
4. Reroute traffic.

The shared volume path:

```
{bundle_server_path}/.transfer/{worker_id}/board.json
```

Mounted read-write into both workers during the handoff window.
Cleaned up after transfer completes.

The only new machinery needed is:

- **Signaling:** the server tells Worker A "flush state to
  `/transfer/board.json`" and Worker B "restore from
  `/transfer/board.json`." A websocket message or HTTP callback
  from the server to the worker's R process.
- **Traffic rerouting:** the proxy switches the route from A to B.
  This already exists in the autoscaling/worker eviction path.

For apps that are NOT blockr (plain Shiny apps), the version
conflict case is a hard restart — session is lost, user reconnects.
This matches the behavior of a normal redeploy.

---

## Design Decisions

1. **pak as the dependency resolver, not renv.** pak has a proper
   constraint solver (ILP via lpSolve), supports three input modes
   (lockfile, DESCRIPTION, script scanning), and bundles all
   dependencies into a single self-contained package. renv lacks a
   solver and is primarily a project isolation tool. renv's global
   cache is appealing, but the renv + pak integration is broken —
   when pak is enabled as renv's install backend, the two cache
   systems don't coordinate (open issues #1846, #1334, #1210).
   Rather than depend on a broken integration, we implement our own
   cache layer inspired by renv's design.

2. **Our own cache layer instead of renv's.** renv's cache is
   designed for interactive development (symlinks, project
   isolation, sandbox, .Rprofile shims). We need server-side build
   caching in containers where none of that applies. Our store uses
   hard links (self-contained views, no runtime dependency on the
   store mount), renv-compatible hashes (same key algorithm), and
   integrates directly with pak's install plan. The store is also
   shared across all apps on the server, not per-project.

3. **Intercept pak's install plan, not `install.packages()`.** pak
   never calls `install.packages()` — it has its own install
   pipeline. The only way to integrate caching is at pak's level:
   inspect the install plan after solving, pre-populate cache hits,
   and mark them as `"current"` so `install_package_plan()` skips
   them. This is a single solve step with no redundant work.

4. **Persistent pak download cache across builds.** pak's pkgcache
   stores downloaded archives keyed by URL + ETag. Mounting a
   persistent directory at `PKG_CACHE_DIR` across builds avoids
   re-downloading packages that haven't changed upstream. This is
   orthogonal to the store (which caches *installed* packages) —
   the download cache helps even for store misses.

5. **renv-style hash as cache key.** MD5 of selected DESCRIPTION
   fields, matching renv's algorithm. This means the manifest's
   `Hash` field (computed client-side or after the first build) is
   the store lookup key. Same-version packages from different
   sources (CRAN vs GitHub) get different hashes because the
   `Remote*` fields differ.

---

## Open Questions

1. **Manifest signing.** Should the CLI sign the manifest so the
   server can verify it wasn't tampered with in transit? Relevant
   when the upload path is not fully trusted (e.g., CI pipelines
   with shared credentials).

2. **Incremental uploads.** The `files` section with checksums
   enables Connect-style incremental deploys (only upload changed
   files). Worth implementing in v2 or defer?

3. **In-container install for cache misses.** Adding pak to the
   worker image enables self-healing for missing packages but
   increases image size. The alternative (background build) is
   cleaner but slower. Decide based on real-world usage of blockr
   boards that pull in unexpected packages.

4. **Board flush signaling.** The container transfer protocol needs
   a concrete mechanism for the server to signal "flush now" to the
   R process inside the worker. Options: (a) websocket message on
   an existing Shiny connection, (b) HTTP endpoint exposed by a
   blockyard R helper loaded in the worker, (c) file-based signal
   (touch a sentinel file, R polls for it). Option (b) is cleanest
   but requires the helper package.

5. **Multi-language support.** This document covers R only. Python
   and Julia support (if added) would follow similar patterns but
   with different tools (uv/pip for Python, Pkg.jl for Julia).
   The manifest format should be extensible to accommodate this.

6. **`install_package_plan()` with modified `lib_status`.** The
   store-aware build flow relies on setting `lib_status = "current"`
   for pre-populated packages so `install_package_plan()` skips
   them. The pkgdepends source shows it marks `"current"` packages
   as `install_done = TRUE` at init, but this needs validation
   with actual pak/pkgdepends versions to confirm there is no
   secondary check that would override our modification.

7. **Download waste for store hits.** The current flow calls
   `proposal$download()` before inspecting the plan, which
   downloads archives for packages we already have in the store.
   With the persistent download cache this is fast (ETag checks,
   no actual transfer), but it could be eliminated by inspecting
   the plan before downloading and selectively downloading only
   cache misses. Worth the complexity?
