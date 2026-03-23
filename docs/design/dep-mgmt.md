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
