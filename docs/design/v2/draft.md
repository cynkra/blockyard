# blockyard v2 — Draft Notes

v2 focuses on single-node production completeness: usability improvements,
safety nets, and blockr-specific features that make the platform pleasant to
operate and use on Docker. Kubernetes and multi-node concerns are deferred to
v3.

## Features

### CLI Tool

A dedicated Go binary for interacting with the server: deploy apps, list
content, tail logs, manage settings. Communicates via the REST API. This is
the single biggest usability improvement for operators and CI/CD pipelines —
right now deployment is raw `curl`.

### Bundle Rollback

Activate a previous bundle for a content item. Drain active sessions
gracefully before switching. The schema already supports multiple bundles per
app with configurable retention — the new work is the rollback API endpoint
and session drain logic.

### Soft-Delete for Apps

Mark apps as deleted instead of immediate removal. A background cleanup
routine purges deleted apps after a configurable retention period. Enables
undo and audit trails. Implementation: add a `deleted_at` column, filter
queries, background sweeper.

### Per-Content Resource Limit Enforcement

CPU and memory limit fields are already in the content registry and carried
in `WorkerSpec` from v0. v2 enforces them at the Docker level via
`--memory` and `--cpus` container creation flags. When the Kubernetes backend
arrives in v3, enforcement comes for free via pod resource specs.

### Scale-to-Zero

When an app has no active connections for a configurable idle period, stop
its workers to free resources. On the next request, hold the connection and
spin up a worker before forwarding. The proxy already implements
request-holding during cold start (`worker_start_timeout`), so the new work
is idle detection and worker teardown.

### Seat-Based Pre-Warming

Pre-start a standby worker per app so the first user doesn't incur
cold-start latency. When a session claims the warm container, replace it
with a fresh one. On a single node with a handful of apps, the resource
cost is minimal — one idle container per app.

### Multiple Execution Environment Images

Per-app image selection. Add an `image` field to app configuration that
overrides the server-wide `[docker] image` default. Operators or app
developers specify which image to use per deployment.

This interacts with the `build_image` config: rv's library path is
namespaced by R version/arch/codename, so the build image's R version must
match the worker image. For v2, the fix is to mount the `rv` binary from
the server container into build containers (bind mount to
`/usr/local/bin/rv`), collapsing back to a single image for builds. This
works with Docker DooD; the Kubernetes variant (init container or shared
volume) is a v3 concern.

### Web UI Expansion

The v1 minimal UI covers the dashboard, user management, PAT management,
and credential enrollment. v2 adds:

- **Content browser** — richer app listing with search/filter, tag
  management UI
- **Log viewer** — live-streaming and historical log viewing per app

Lower priority than the other v2 items since the API + CLI covers the same
functionality.

### Runtime Package Installation

A blockr app is built from **blocks**, which are defined in R packages. The
set of available blocks is currently fixed at deploy time. Users need to
install additional block packages during a live session.

#### Design: Package Store + Hard-Linked Views

The design separates three concerns: (1) a server-level **package store**
that holds every package version ever installed, (2) per-worker **library
views** that expose only the packages a given worker needs, and (3) an
**API** that Shiny/blockr calls to request packages. This keeps blockyard's
responsibility narrow (cache management, view construction) and blockr's
responsibility where it belongs (knowing which packages are needed and when).

##### Package Store

A content-addressable directory on the host, organized by package name,
version, and source. Not an R library — R never sees this directory directly.

```
{bundle_server_path}/.pkg-store/
├── ggplot2/
│   ├── 3.4.0-cran/          ← installed R package tree
│   └── 3.5.0-cran/
├── blockr.ggplot/
│   ├── 0.2.0-cran/
│   └── 0.2.1-gh-blockr/     ← same version, different source
├── blockr.dplyr/
│   └── 0.1.0-cran/
└── ...
```

The store grows monotonically. When a package is requested that doesn't
exist in the store, blockyard installs it via a build container. Packages
are never modified after installation — the store is append-only. Garbage
collection (removing versions no longer referenced by any board or app) is a
separate, future concern.

Multiple versions of the same package coexist. Multiple variants of the same
version (e.g., CRAN vs. GitHub) coexist, distinguished by their source
suffix.

##### Per-Worker Library Views

Each worker gets a flat R library directory on the host, populated with
**recursive hard links** into the package store:

```
{bundle_server_path}/.worker-libs/{worker-id}/
├── ggplot2/     ← hard-linked tree from .pkg-store/ggplot2/3.5.0-cran/
├── blockr.ggplot/ ← hard-linked tree from .pkg-store/blockr.ggplot/0.2.0-cran/
└── ...
```

The worker container mounts this directory read-only at `/extra-lib/`. R's
`.libPaths()` is set to `c("/extra-lib/", "/rv-library")` — the view takes
precedence so that live-installed packages can shadow older versions in the
base library (see "Library Path Ordering" below).

**Hard links** (`cp -al`) create the directory structure with zero additional
disk usage for file contents — every file shares the same inode and disk
blocks as the original in the package store. Creating a hard-linked tree for
a typical R package takes milliseconds. Cleanup on worker shutdown is
`rm -rf` on the view directory, which removes only the hard links; the store
is unaffected.

Constraint: hard links require the package store and worker-libs directories
to be on the same filesystem. This is naturally satisfied when both live
under `bundle_server_path`.

**Why hard links over alternatives:**

| Approach                        | Disk cost | Speed     | Privileges | Complexity |
|---------------------------------|-----------|-----------|------------|------------|
| Full copy                       | Full      | Slow      | None       | Low        |
| Hard links (`cp -al`)           | ~Zero     | Fast      | None       | Low        |
| Reflinks (`cp --reflink`)       | ~Zero     | Fast      | None       | Low (but requires btrfs/XFS) |
| Bind mounts + slave propagation | Zero      | Instant   | Root/CAP_SYS_ADMIN | Medium (mount lifecycle) |
| Symlinks into mounted cache     | Zero      | Instant   | None       | Medium (requires cache mounted in container) |

Hard links hit the best trade-off: near-zero cost, no special privileges, no
filesystem requirements beyond "same mount point," no Docker configuration
changes, no mount lifecycle management. The view directory is a plain bind
mount — the existing Docker backend code handles it without modification.

##### Library Path Ordering

The worker's `.libPaths()` is set to `c("/extra-lib/", "/rv-library")` —
the live-install view takes precedence over the bundle's base library. This
ordering is critical: when a live-installed package provides a newer version
of something already present in the base library, R must find the newer
version first. With the reverse ordering (`/rv-library` first), a newer
`ggplot2` in `/extra-lib/` would be shadowed by the older one in the base
library, silently causing version mismatches.

##### Adding Packages to a Running Worker

Because the worker-libs directory is bind-mounted into the container, **new
hard-linked packages created on the host are immediately visible inside the
container.** This is standard bind mount behavior — no mount propagation
configuration needed.

However, R namespaces are **immutable once loaded** — `library()` is a
no-op for a package whose namespace is already attached, and
`unloadNamespace()` fails if anything imports it (which is nearly always
the case for foundational packages). This means a live-installed package
that requires a newer version of an already-loaded dependency cannot be
used without restarting the R session.

This gives three cases:

| Situation | Already loaded? | Action | User impact |
|---|---|---|---|
| Purely additive — not in base library, not loaded | No | Hard-link into view, `library()` | Seamless |
| Shadows base library, not yet loaded | No | Hard-link into view, `library()` loads new version | Seamless |
| Shadows base library or extra-lib, already loaded | Yes | Save board state, restart session | Brief interruption |

The first two cases are the common path. The third — a loaded namespace
that needs upgrading — requires a session restart (see "Session Restart
on Conflict" below).

**The sequence when blockr requests a package mid-session (cases 1 and 2):**

1. blockr resolves the dependency tree for the requested package and checks
   it against `loadedNamespaces()`. If any loaded namespace would need
   upgrading → divert to the restart flow (case 3).
2. blockr calls `POST /api/v1/packages` with the package name (and
   optionally version/source).
3. blockyard checks the package store. If present → skip to step 5.
4. If not present → spawn a build container to install the package into the
   store (see "Installation Flow").
5. Create a hard-linked tree from the store entry into the requesting
   worker's view directory.
6. Return success.
7. blockr calls `library(pkg)` — R finds it in `/extra-lib/`.

Step 5 (hard-linking) takes milliseconds. Step 4 (installation) takes
seconds to minutes depending on the package, but only happens once per
package version globally. Subsequent requests for the same package from any
worker skip straight to step 5.

##### Session Restart on Conflict

When blockr detects that a requested package's dependency tree conflicts
with an already-loaded namespace (case 3), the install cannot proceed
in-process. Instead:

1. blockr serializes the current board state (the same mechanism used for
   board save).
2. blockr calls the install API — blockyard installs the package into the
   store and updates the worker's view as usual.
3. blockr signals blockyard (or the Shiny session) that a restart is
   needed. The exact mechanism is TBD — possibilities include a
   `POST /api/v1/workers/{id}/restart` endpoint or a client-side
   `session$reload()`.
4. The worker restarts with the updated view already in place (hard links
   are persistent). R starts fresh with all packages available at the
   correct versions.
5. blockr restores the saved board state. The user is back where they were,
   with the new block available.

Users already expect this interaction model — installing a package in
RStudio that conflicts with a loaded namespace produces a "please restart
R" prompt. The experience here is similar but more seamless because
blockyard handles the library reconstruction and blockr handles the
state preservation automatically.

##### Why Conflicts Are Rare in Practice

Three factors conspire to keep the restart path uncommon:

1. **Block packages are lightweight.** Block packages are thin wrappers
   defining block interfaces. Their dependency trees are small and largely
   overlap with the dependencies the app already has — they don't
   introduce heavy new dependency sub-trees that demand version bumps.

2. **A current base library eliminates version lag.** If the bundle's base
   library is restored from a recent lockfile, its packages are already at
   or near the latest versions. A live-installed block package is unlikely
   to demand a *newer* version of something the base library already
   provides at the latest release. R's dependency system has no upper
   bounds on version constraints — `Depends: ggplot2 (>= 3.4.0)` is
   satisfied by any newer version, so a current base library satisfies
   virtually all downstream constraints.

3. **PPM snapshots guarantee internal consistency.** When both the base
   library (restored by `rv` at deploy time) and live installs (via `pak`
   at runtime) resolve against the same Posit Package Manager snapshot
   date, the entire dependency graph is drawn from a single coherent
   package universe. There is no version where the live install wants
   `ggplot2 3.6.0` but the base library has `3.5.0` — they both resolve
   to whatever version the snapshot contains. Conflicts are limited to
   packages sourced outside the snapshot (e.g., GitHub remotes with
   pinned dependency floors above the snapshot's versions).

   Configuring a PPM snapshot URL is the single most effective mitigation
   and should be the recommended default for production deployments.

##### Board Save and Restore

This architecture dissolves the board restore timing problem. Board restore
no longer requires pre-provisioning a custom library before starting the
worker — the worker is already running with the view directory mounted.

**Save:** When a board is saved, blockr records the full dependency set
(package names, versions, sources) alongside the board data. This is the
union of the app's base packages and any extras that were live-installed
during the session.

**Restore:** When a board is loaded, blockr checks which packages are
already available (in `/rv-library` or `/extra-lib/`) and requests any
missing ones via the API. Since the packages likely already exist in the
store (they were installed at least once before), this is a hard-link
operation — near-instant. If all missing packages are purely additive
(no loaded-namespace conflicts), the app does not need to restart. If
any conflict with a loaded namespace, the session restart flow handles
it transparently — the board being restored is itself the state that
gets carried across the restart.

**Fallback for boards with no dependency metadata** (e.g., boards saved
before this feature exists): blockr attempts to load blocks, catches
failures from missing packages, and calls the API for each. Slower (serial
install-on-demand) but functional.

##### Installation Flow

When the package store doesn't have a requested package:

1. blockyard spawns a build container using the same base R image.
2. The container mounts the package store directory read-write.
3. The container runs a targeted install — e.g.,
   `pak::pkg_install("blockr.ggplot", lib = "/pkg-store/blockr.ggplot/0.2.0-cran")`.
4. `pak` resolves transitive dependencies. Deps that already exist in the
   store are skipped; new deps are installed into their own store entries.
5. On success, the container exits and blockyard records the package in its
   index.

`pak` is the likely tool here — it handles CRAN, Bioconductor, GitHub, and
other sources, resolves dependencies efficiently, supports binary packages
from P3M, and has a clean programmatic API. Neither rv nor renv is designed
for this incremental "add one package" workflow.

**Transitive dependency handling** is the subtle part. When
`blockr.ggplot 0.2.0` depends on `ggplot2 >= 3.4.0`, and the store already
has `ggplot2 3.5.0-cran`, the install should reuse it rather than installing
a duplicate. The build container needs visibility into what the store already
holds. One approach: mount the store read-only as an additional library path
during the build, so `pak` sees already-installed packages and only installs
the delta.

##### Separation of Responsibilities

| Concern                         | Responsible     | Mechanism                            |
|---------------------------------|-----------------|--------------------------------------|
| Knowing which packages are needed | blockr (R)    | Block explorer, board metadata       |
| Requesting a package            | blockr (R)      | `POST /api/v1/packages`              |
| Installing into the store       | blockyard (Go)  | Build container with `pak`           |
| Constructing worker views       | blockyard (Go)  | Hard-linked trees                    |
| Loading packages into R session | blockr (R)      | `library()` against `.libPaths()`    |
| Deciding package versions       | TBD             | See open questions                   |

#### rv vs. renv

Both tools are relevant but at different points in the lifecycle:

| Concern                  | rv                                     | renv                                    |
|--------------------------|----------------------------------------|-----------------------------------------|
| Dependency discovery     | Not supported                          | `renv::dependencies()` — static analysis of R scripts for `library()`, `require()`, `pkg::fn()` |
| Dependency resolution    | From lockfile only                     | `renv::snapshot()` — resolves full dependency graph with versions |
| Package installation     | Fast restore from lockfile             | `renv::restore()` — install from lockfile |
| Incremental install      | Not designed for it                    | Not designed for it                     |

The gap that has already bitten us: **rv cannot infer dependencies from
source code.** On the bundle-creation side (client tooling), renv's static
analysis is still needed. rv's strength is fast, reliable restoration from a
fully-resolved lockfile — which is exactly what the server-side build step
needs.

A possible split:
- **Client-side (bundle creation):** renv for dependency discovery and
  snapshot, producing a lockfile.
- **Server-side (bundle restore):** rv for fast installation from that
  lockfile. This is what blockyard already does.
- **Server-side (live install):** New mechanism — a thin shim over
  `pak::pkg_install()` rather than either rv or renv.

#### Open Questions

1. **Board serialization format.** Does the current blockr format capture
   which packages each block comes from, or only block identifiers? What
   changes are needed on the blockr side to record package name + version +
   source alongside board data?

2. **Version resolution policy.** When a user requests "blockr.ggplot"
   without specifying a version, what does blockyard install? The
   recommended default is to resolve against a configured PPM snapshot URL
   (the same snapshot used for bundle restoration). This guarantees
   consistency between base and live-installed packages. The API should
   accept an optional version/source but default to the configured
   repository set. Open sub-question: should blockyard enforce that the
   live-install repository configuration matches the base library's
   snapshot, or allow operators to diverge?

3. ~~**Transitive dependency version conflicts.**~~ Resolved — see
   "Library Path Ordering", "Session Restart on Conflict", and "Why
   Conflicts Are Rare in Practice" above. Summary: `/extra-lib/` takes
   precedence in `.libPaths()` so newer versions shadow the base library
   for not-yet-loaded packages. Already-loaded namespaces that need
   upgrading trigger a board-state-preserving session restart. PPM
   snapshots, current base libraries, and lightweight block packages make
   the restart path uncommon in practice.

4. **Security.** Installation happens in a sandboxed build container (not in
   the running worker), which limits the blast radius. But the installed
   package then runs inside the worker with the user's session context.
   Should there be an allowlist of installable packages? Per-app
   restrictions? Admin-only approval for new packages?

5. **Store garbage collection.** The store grows monotonically. When should
   packages be removed? Options: reference counting (remove when no board or
   active worker references a version), TTL-based expiry, manual admin
   cleanup, or never (disk is cheap). This is not urgent but needs a policy
   eventually.

6. **API design.** Should the install endpoint be synchronous (block until
   package is ready) or async (return a task ID, client polls)? For packages
   already in the store, the response is near-instant. For new installs, it
   could take minutes. A hybrid approach — return immediately if cached,
   else return a task ID — may be cleanest.

7. **Impact on cold-start time.** When a worker starts and needs extra
   packages (e.g., for a board restore), hard-linking is fast. But if any
   required package is missing from the store and must be installed first,
   the user-facing delay could be significant. Should blockyard pre-warm the
   store based on known board dependencies? Or is the expectation that most
   packages are already cached from prior sessions?

8. **Interaction with R version upgrades.** R packages compiled for one R
   version may not work with another. If the server-wide R image is
   upgraded, does the entire store need to be invalidated and rebuilt? The
   store key should probably include the R version (e.g.,
   `ggplot2/3.5.0-cran-R4.4/`).
