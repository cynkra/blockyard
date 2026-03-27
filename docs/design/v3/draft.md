# blockyard v3 — Draft Notes

v3 has two tracks: the lightweight process backend (the headline feature)
and deferred single-node features that didn't make the v2 cut. The process
backend is the architectural work; the deferred items are scoped extensions
to the existing Docker deployment.

## Process Backend

See [backends.md](../backends.md) for the full design and isolation
analysis.

### Process Backend Implementation

Implement the `Backend` interface using bubblewrap (`bwrap`) for process
sandboxing. Workers are spawned as bwrap-sandboxed R processes with PID
namespace isolation, filesystem isolation via bind mounts, seccomp
filtering, and capability dropping. No per-worker network isolation or
resource limits — these are deferred to the Docker and Kubernetes backends.

### Containerized Deployment Mode

A Docker image shipping blockyard, R, bwrap, and system libraries. Runs
with a custom seccomp profile allowing user namespace creation — no Docker
socket, no `CAP_SYS_ADMIN`. The recommended deployment mode for most users
of the process backend. Provides rootfs containment: a bwrap sandbox escape
lands in the outer container's filesystem, not the host.

### Native Deployment Mode

Documentation and tooling for running the process backend directly on a
Linux host without an outer container. Operator provisions R, bwrap, and
system libraries.

**CI note:** The release workflow currently produces a single
`linux/amd64` binary. For the native deployment mode, multi-platform
release binaries will be needed (`linux/amd64`, `linux/arm64`,
`darwin/amd64`, `darwin/arm64`). Go cross-compilation makes this
straightforward — no special runners required.

### Custom Seccomp Profile

A JSON seccomp profile based on Docker's default, adding `CLONE_NEWUSER`
to the allowlist. Shipped alongside the Docker Compose configuration.

## Pre-Fork Worker Model

An enhancement to the Docker backend's multi-session mode
(`max_sessions_per_worker > 1`). Instead of multiplexing sessions onto a
shared R process — where sessions share memory, global state, and have
zero isolation from each other — each user gets their own R process
inside the container, forked from a pre-loaded template.

### Motivation

The current multi-session model mirrors Posit Connect and ShinyProxy:
multiple sessions share a single R process. This is memory-efficient
but provides no isolation between users — one session can read another's
reactive values, global variables, temp files, and environment
variables. A crash in one session kills all co-tenants.

The opposite extreme — one container per user — provides full isolation
but carries ~500ms–1s startup overhead per user and significant memory
duplication (each R process independently loads the same packages).

The pre-fork model is a middle ground: the container boundary provides
inter-app isolation (network, rootfs, resource ceiling), while per-user
forked processes provide intra-app isolation (separate memory, crash
isolation, filesystem separation) with copy-on-write memory sharing.

### Mechanism

**Template (zygote) process.** When a worker container starts, a
template R process loads all packages required by the app — including
Shiny and httpuv — but does **not** call `shiny::runApp()`. httpuv's
libuv event loop is lazily initialized only when `startServer()` is
called, so `library(shiny)` does not create background threads or open
sockets. The template process is fork-safe at this point.

**Fork per user.** When a new user session arrives, the template process
is `fork()`'d. The child inherits all loaded packages via copy-on-write
— zero additional memory for read-only pages (function definitions,
bytecode, compiled shared libraries). The child then calls
`shiny::runApp()` on its own port, creating a fresh libuv event loop
and httpuv server. The server proxies traffic to each child's port.

**Post-fork sandboxing.** After `fork()`, each child applies isolation
before starting Shiny:

1. `unshare(CLONE_NEWUSER | CLONE_NEWNS)` — creates a new user and
   mount namespace. Requires the container to run with a custom seccomp
   profile (same one the process backend needs) and `apparmor=unconfined`
   on Ubuntu 23.10+ hosts.
2. Mount a private tmpfs at `/tmp` — prevents cross-user temp file
   access. R's `tempdir()` resolves to the private mount.
3. `prctl(PR_SET_NO_NEW_PRIVS, 1)` + seccomp-bpf filter — restricts
   available syscalls.
4. `capset()` — drops all capabilities.
5. `setrlimit()` — per-process resource limits: `RLIMIT_AS` (virtual
   memory), `RLIMIT_CPU` (CPU time), `RLIMIT_NPROC` (fork limit).

### Isolation Properties

| Property | Mechanism | Strength |
|---|---|---|
| Memory | Separate R heaps (CoW from template) | Full — no cross-session state access |
| Crash | Separate processes | Full — one child's crash doesn't affect others |
| Filesystem | Private mount namespace, private `/tmp` | Full — each child has its own tmpfs |
| Syscalls | Per-child seccomp-bpf filter | Full |
| Capabilities | Per-child capability drop | Full |
| Resources | `setrlimit()` per child; container cgroup as shared ceiling | Partial — `RLIMIT_AS` caps virtual memory per child, but no cgroup-level enforcement per child |
| PID | Shared PID namespace within container | Weak — children can see each other via `/proc` and signal each other if same UID |
| Network | Shared network namespace within container | None — children can connect to each other's ports on localhost |

**Comparison with shared-process model:** every row except Network is a
strict improvement. The shared-process model provides none of these
isolation properties. The PID and Network gaps are residual — PID
visibility is mitigatable by running children as different UIDs or
restricting `/proc` access; the network gap is low-risk because all
children run the same app code (the threat is data leakage between
sessions, which is addressed by memory and filesystem isolation, not
malicious code exploiting network access).

### Memory Efficiency

Copy-on-write sharing from `fork()` is the key to making per-user
processes viable. The memory profile:

- **Shared (zero cost per child):** compiled shared libraries (`.so`
  files) are deduplicated by the kernel page cache automatically.
  R-level package namespaces, function definitions, and bytecode are
  shared via CoW from the template.
- **Per-child overhead:** session-specific state (reactive values, user
  data, plot objects), R garbage collector activity (triggers CoW page
  duplication as pages are modified), and Shiny/httpuv event loop
  structures.

For typical Shiny apps, the bulk of memory is package code (read-only
after loading), so CoW sharing is highly effective. Calling `gc()` in
the template before forking minimizes GC-triggered page duplication in
children.

**Prior art:** Rserve, RestRserve, and OpenCPU all use this pattern in
production. Rserve pre-loads packages in a parent process and forks per
connection. OpenCPU runs on Apache prefork MPM with pre-loaded R
packages shared via CoW.

### Docker Configuration

The container requires two non-default security options:

**Custom seccomp profile.** Docker's default profile blocks
`CLONE_NEWUSER` and `unshare`. The custom profile (same one shipped for
the process backend) modifies the clone mask to remove the
`CLONE_NEWUSER` bit and adds an unconditional allow for `unshare`.

**AppArmor override (Ubuntu 23.10+ hosts only).** Ubuntu's default
AppArmor policy blocks unprivileged user namespace creation
independently of seccomp. Override with `apparmor=unconfined` or a
custom AppArmor profile that includes the `userns,` rule.

```
docker run \
  --security-opt seccomp=blockyard-seccomp.json \
  --security-opt apparmor=unconfined \
  ghcr.io/cynkra/blockyard:latest
```

No `--privileged`, no `--cap-add SYS_ADMIN`, no Docker socket
differences from the standard Docker backend deployment.

### Package Compatibility

Packages fall into three categories for fork safety:

**Safe to pre-load** (the typical Shiny stack): shiny, httpuv, ggplot2,
dplyr, tidyr, DT, plotly, leaflet, htmlwidgets, and most pure-R
packages. These do not spawn threads or open connections at load time.

**Dangerous to pre-load** (spawn threads at load time):

| Package | Issue | Mitigation |
|---|---|---|
| data.table | Initializes OpenMP thread state | Built-in: auto-drops to 1 thread in forked children |
| arrow | Starts CPU and I/O thread pools | Do not pre-load; load in each child |
| torch | Initializes LibTorch runtime | Do not pre-load; load in each child |
| rJava | Starts JVM | Completely fork-incompatible; load in each child |
| reticulate | Python initialization | Do not pre-load if Python is initialized |

**Safe to pre-load, dangerous if used before fork:** DBI, RPostgres,
RSQLite, httr, httr2, curl. These only create connections on explicit
use (`dbConnect()`, `GET()`), not at `library()` time. Pre-loading is
safe; opening connections before forking is not.

**Environment variable hardening:** set `OMP_NUM_THREADS=1` and
`MKL_NUM_THREADS=1` in the template process before forking to prevent
multi-threaded BLAS deadlocks in children.

### Additional Caveats

- **RNG seeding.** Forked children inherit the parent's RNG state. Each
  child must re-seed independently — e.g., `set.seed()` incorporating
  the PID, or `RNGkind("L'Ecuyer-CMRG")` for statistically independent
  streams.
- **GC-driven CoW growth.** R's garbage collector modifies pages
  (reference counts, free lists), triggering copy-on-write duplication.
  Memory per child grows over time as GC runs. This is the same
  tradeoff Rserve and OpenCPU make. Calling `gc()` in the template
  before forking reduces the initial duplication surface.
- **`tempdir()` inheritance.** Parent and children share the same
  `tempdir()` path by default. The private mount namespace (step 2 of
  post-fork sandboxing) addresses this — each child's `/tmp` is
  independent.

### Open Questions

1. **Template process management.** The template process runs inside the
   container but is not itself a Shiny server. The blockyard server
   needs a mechanism to signal "fork a new child on port N" — likely
   via `docker exec` into the running container, or a lightweight
   control socket in the template process. The `docker exec` approach
   avoids an in-container agent; the control socket approach avoids
   per-user Docker API calls.

2. **Port allocation.** Each forked child listens on its own port inside
   the container. The server allocates ports from a range and proxies
   to `container_ip:port`. Port exhaustion is bounded by
   `max_sessions_per_worker`.

3. **Child lifecycle.** When a user disconnects, the forked child should
   exit (after the WebSocket cache TTL grace period). The template
   process or a reaper must detect and clean up exited children.
   `waitpid()` with `WNOHANG` or `SIGCHLD` handling in the template.

4. **Interaction with scale-to-zero.** When all children exit, the
   container has only the idle template process. The server can either
   keep the container alive (warm template, instant fork on next user)
   or stop it (full scale-to-zero, cold start on next user).
   Keeping it alive is the natural default — the template's memory
   footprint is the cost of one R process.

## Deferred Single-Node Features

### Data Mounts

App owners can map additional host directories into worker containers —
e.g., shared model files, reference datasets, or writable scratch space.

#### Mount Specification

Each mount has three fields:

| Field      | Required | Description                                      |
|------------|----------|--------------------------------------------------|
| `source`   | yes      | Named mount source (defined by admin), optionally followed by a subpath — e.g., `models` or `models/v2` |
| `target`   | yes      | Absolute path inside the worker container — e.g., `/data/models` |
| `readonly` | no       | Default `true`. Set to `false` for writable mounts. |

Stored as a JSON column (`data_mounts`) on the `apps` table, consistent
with other per-app settings like `memory_limit` and `cpu_limit`.

#### Admin-Defined Mount Sources

The server config defines a whitelist of named mount sources. App owners
can only mount from these sources — they never specify raw host paths.

```toml
[[storage.data_mounts]]
name = "models"
path = "/data/shared-models"

[[storage.data_mounts]]
name = "scratch"
path = "/data/scratch"
```

`path` is from the server's perspective (i.e., inside the server
container in a DooD setup). The existing `MountConfig` logic in
`mounts.go` handles translation to the host-side path before passing it
to the Docker API — the same mechanism that already translates bundle
paths across native, bind-mount, and volume deployment modes.

When an app owner specifies `source: "models/v2"`, the server:
1. Splits into source name (`models`) and subpath (`v2`).
2. Validates the source name against the configured mount sources.
3. Validates the subpath contains no `..` components (path traversal).
4. Resolves the full server-side path (`/data/shared-models/v2`).
5. Translates to the host-side path via `MountConfig`.
6. Adds the bind mount to the worker container spec.

This gives operators full control over which host directories are
exposed, while app owners get a portable, environment-agnostic
interface. The same app config works across staging and production —
only the admin-defined paths differ.

#### Interaction with Existing Mounts

Worker containers already receive two read-only mounts: the bundle at
`/app` and the restored library at `/blockyard-lib`. Data mounts must
not collide with these paths or with each other. The server rejects
mount specs where `target` conflicts with a reserved path.

### Docker Daemon Hardening

The server communicates with the Docker daemon via the Docker socket.
A compromised server process has unrestricted access to the Docker API —
it could create privileged containers, mount arbitrary host paths, use
host networking, or mount the Docker socket itself into a worker. Data
mounts increase the mount surface area, making this more visible.

#### Docker Authorization Plugins

Docker supports [authorization plugins](https://docs.docker.com/engine/extend/plugins_authorization/)
that intercept every API request before the daemon processes it. An
authorization plugin can inspect the request (image, mounts, network
mode, capabilities, etc.) and return allow or deny.

The policy for blockyard workers should enforce:

- **Mount sources** are restricted to configured data mount paths,
  bundle storage, and library paths. No access to `/`, `/etc`,
  the Docker socket, or other sensitive host paths.
- **No privileged containers.** Workers must never run with
  `--privileged` or elevated capabilities.
- **No host networking.** Workers use the blockyard-managed bridge
  network only.
- **Image allowlist.** Workers may only use the server-wide default
  image or explicitly configured per-app images.

This can be implemented as a purpose-built plugin (a small HTTP server
that parses Docker API create-container requests and validates against
the policy) or via an existing OPA-based plugin like `opa-docker-authz`
with a Rego policy encoding the above rules.

The authorization plugin runs as a daemon-level configuration — the
operator enables it in Docker's `daemon.json`. This means enforcement
is independent of blockyard's application code: even if the server is
fully compromised, the Docker daemon itself refuses to create containers
that violate the policy.

#### Residual Risk: Shared Kernel

The authorization plugin, combined with the existing worker hardening
(`--cap-drop=ALL`, `no-new-privileges`, read-only root, default seccomp
profile, per-container bridge networks), closes off misconfiguration and
server-compromise attack paths. The residual risk is the shared host
kernel — the irreducible attack surface of container-based isolation.

Known container escape vectors against hardened containers:

- **Kernel local privilege escalation.** The container shares the host
  kernel. A bug in any reachable kernel code path (namespaces, cgroups,
  filesystems, netfilter) can break out. These appear several times per
  year — e.g., CVE-2022-0185 (user namespace + filesystem), CVE-2024-1086
  (nftables), CVE-2022-0847 Dirty Pipe. Seccomp reduces the reachable
  syscall surface but the kernel still exposes hundreds of entry points.
- **Container runtime bugs.** Vulnerabilities in runc or containerd
  itself — e.g., CVE-2019-5736 (runc binary overwrite), CVE-2024-21626
  Leaky Vessels (runc working directory file descriptor leak). Rarer but
  high-impact.

Keeping the host kernel and Docker/runc patched is the single most
important operational measure — it matters more than any additional
configuration-level hardening.

#### Stronger Isolation: Alternative Runtimes

For internet-facing deployments where adversarial input is expected,
Docker supports swapping the container runtime without changing
application code or the blockyard Docker backend. The OCI runtime
interface means this is purely a deployment-time configuration choice.

| Runtime | Mechanism | Overhead | Isolation boundary |
|---|---|---|---|
| **runc** (default) | Shared kernel, namespaces + seccomp | Negligible | Kernel syscall surface — hundreds of entry points |
| **gVisor (runsc)** | User-space kernel reimplementation; intercepts syscalls in a Go process before they reach the host kernel | ~5–15% CPU | gVisor's own codebase — most host kernel exploits don't apply, but gVisor itself can have bugs, and a handful of syscalls still pass through to the host |
| **Kata Containers** | Real VM per container; guest runs its own kernel, host kernel is only reachable through the hypervisor's virtual device interface (virtio) | ~30–50MB RAM per VM, ~100–200ms additional boot | Hypervisor virtual device interface — escapes require a hypervisor vulnerability, which are dramatically rarer and harder than kernel privilege escalations |

**Kata Containers** is the recommended runtime for public-internet
deployments. It is an OCI-compatible runtime — configured in Docker's
`daemon.json` or per-container via `--runtime=kata-runtime`. The
existing Docker backend code requires no changes: networking, mounts,
resource limits, labels, and log streaming all work through Docker's
API as before.

The performance tradeoff is well-suited to Shiny workloads. The
per-VM memory overhead (30–50MB) is modest relative to an R process,
and the additional boot latency (100–200ms) is invisible against a
Shiny cold start that already takes seconds for R initialization and
package loading.

gVisor is a lighter alternative that significantly raises the bar
over runc (used by Google for Cloud Run and GKE Sandbox), but its
isolation boundary is weaker than Kata's — it filters syscalls in
userspace rather than interposing a real VM boundary. For deployments
where the additional per-VM memory cost of Kata is acceptable, Kata
provides a stronger guarantee.

#### Per-App Runtime Selection

Docker allows overriding the runtime per container via the
`--runtime` flag, independent of the daemon-wide default. This means
a single blockyard instance can run different apps at different
isolation levels — e.g., Kata for public-facing apps that accept
untrusted input and runc for private apps behind authentication.

Add a `runtime` field to per-app configuration, following the same
pattern as the existing `image`, `memory_limit`, and `cpu_limit`
fields. When set, the Docker backend passes it through to the
container create call. When unset, Docker uses its configured default
runtime.

```toml
[docker]
runtime = "kata-runtime"   # server-wide default

# Per-app override (in app config / database):
# runtime = "runc"         # cheaper isolation for trusted/private apps
```

The authorization plugin policy should allowlist the set of permitted
runtimes (e.g., `["runc", "kata-runtime"]`) to prevent a compromised
server from selecting an unrecognized or weaker runtime.

Documenting the Kata runtime swap, per-app runtime configuration, and
verifying Shiny workload compatibility is a low-effort addition to the
deployment guide.

### App Rename

Renaming an app changes its URL (`/app/{name}/`), which breaks active
sessions, WebSocket connections, and path-scoped cookies. A safe
implementation needs a drain-and-redirect mechanism similar to unpinned
dependency updates: drain existing workers under the old name, redirect
`/app/old-name/*` to `/app/new-name/*` for a grace period, and handle
client-side URL invalidation (sidebar htmx attributes, bookmarks).

Deferred from v2 (removed from phases 2-8 and 2-9) because the
session/cookie breakage cannot be handled gracefully without the
drain-redirect infrastructure.

### Dynamic Resource Limit Updates

v2 enforces resource limits at container creation and validates inputs
at the API boundary, but changing limits via `PATCH /api/v1/apps/{id}`
only affects newly spawned workers. Running workers retain their
original limits.

Docker supports `client.ContainerUpdate()` (maps to
`POST /containers/{id}/update`) to change `Memory`, `NanoCPUs`, and
other resource constraints on a running container without restart. This
is the cleanest path for the Docker backend.

**Implementation sketch:**

1. In `UpdateApp`, when `memory_limit` or `cpu_limit` changes, call a
   new `Backend.UpdateResources(ctx, workerID, limits)` method for each
   running worker.
2. The Docker backend implements this via `ContainerUpdate()`.
3. The process backend (new in v3) implements this via direct cgroup
   writes (`memory.max`, `cpu.max` in cgroup v2).
4. The Kubernetes backend (v4) patches the pod spec — note that
   in-place resource resize is a Kubernetes 1.27+ feature
   (`InPlacePodVerticalScaling` gate) and may not be available on all
   clusters.

**Backend interface addition:**

```go
// UpdateResources changes resource limits on a running worker.
// Returns ErrNotSupported if the backend doesn't support live updates.
UpdateResources(ctx context.Context, id string, mem int64, nanoCPUs int64) error
```

The API handler should call this best-effort — if the backend returns
`ErrNotSupported`, the change is still persisted in the DB and takes
effect on the next spawn. Log a note so the operator knows.

### Multiple Execution Environment Images

Per-app image selection. Add an `image` field to app configuration that
overrides the server-wide `[docker] image` default. Operators or app
developers specify which image to use per deployment.

This interacts with the `build_image` config: rv's library path is
namespaced by R version/arch/codename, so the build image's R version must
match the worker image. The fix is to mount the `rv` binary from the server
container into build containers (bind mount to `/usr/local/bin/rv`),
collapsing back to a single image for builds. This works with Docker DooD;
the Kubernetes variant (init container or shared volume) is a v4 concern.

### UI Branding and Customization

- **Customizable cold-start loading page** — v2 ships a default
  blockyard-branded spinner. v3 makes it configurable (custom HTML,
  logo, messaging).
- **In-app navigation chrome** — navbar, app switcher for navigating
  between deployed apps without returning to the dashboard.
- **General branding** — configurable logo, colors, landing page
  content for the app browser.
