# Phase 3-8: Process Backend Packaging & Deployment

Phase 3-7 implements the process backend's runtime: bwrap-sandboxed
worker processes, port and UID allocation, log capture, preflight
checks, the `Backend` interface decoupling. This phase packages it for
real deployments — the seccomp profiles that make containerized mode
work, the Dockerfiles that ship R + bwrap + the binary, the CI workflow
that publishes multi-arch variant images, the documentation operators
read on bare Linux hosts, and the orchestrator variant that performs
zero-interruption rolling updates without a Docker socket.

It also lands a backend selection refactor that all future backends
will benefit from: build-tag gating so the Docker SDK does not enter
the dependency graph of a process-only image, and vice versa. This
turns the three-image scheme into three honest binaries rather than
three runtime layers wrapping the same Go program.

Depends on phase 3-7 (process backend implementation) and phase 3-5
(Docker rolling update orchestrator — phase 3-8 reuses the cutover,
watchdog, scheduled-update, and `/admin/activate` machinery and only
replaces the "create a new server instance" step).

---

## Prerequisites from Earlier Phases

- **Phase 3-3** — Redis-backed shared state. Both servers in a
  process-backend rolling update read the same `SessionStore`,
  `WorkerRegistry`, and `WorkerMap` so the cutover is non-disruptive.
  Phase 3-8's Redis-backed UID allocator (step 7) also uses this
  connection — one key per allocated host UID, coordinated via Lua
  script. Without Redis, rolling updates are not available —
  `by admin update` prints the manual restart instructions, same as
  the Docker variant. Single-node process-backend deployments fall
  back to the in-memory UID allocator, which is correct because
  without the cutover window there are no cross-server collisions.
- **Phase 3-4** — drain mode, passive mode (`BLOCKYARD_PASSIVE=1`),
  the three-method `Drain()` / `Finish()` / `Shutdown()` lifecycle,
  and `Undrain()`. Phase 3-8 extends `Finish()` with an optional
  idle-wait prelude controlled by a new `FinishIdleWait` field on
  the `Drainer` struct, but leaves the method signatures and call
  sites unchanged.
- **Phase 3-5** — Docker rolling update orchestrator
  (`internal/orchestrator/`), `BackupWithMeta`, `LatestBackupMeta`,
  the `/api/v1/admin/update` / `/admin/rollback` / `/admin/activate`
  endpoints, the activation token mechanism, watchdog and
  scheduled-update flows, the `by admin` CLI subcommand group. Phase
  3-8 refactors this package so the cutover/watchdog/scheduled core
  is backend-agnostic and the Docker-specific clone code becomes one
  of two implementations.
- **Phase 3-7** — process backend implementation: `ProcessBackend`,
  `bwrapArgs`, port and UID allocators, preflight checks, the
  decoupled `Backend.Preflight()` and `Backend.CleanupOrphanResources()`
  interface methods, `applySeccomp` accepting an optional pre-compiled
  BPF profile, and the `blockyard probe` subcommand. Phase 3-8 ships
  the compiled profile that phase 3-7 left as `SeccompProfile = ""`.
  Phase 3-8 also refactors the UID allocator — phase 3-7's in-memory
  implementation remains as one of two backends.
- **Issue cynkra/blockyard#173** — tightens the port allocator's
  `Alloc`→`cmd.Start` race window by holding the listener through
  setup and closing it immediately before fork. Independent of phase
  3-8 scope; filed because phase 3-8's rolling update makes the
  existing pre-phase-3-8 race more visible. Phase 3-8 assumes this
  fix is in place and layers the Redis-backed port allocator on top
  — #173 narrows the local window to microseconds, Redis closes the
  cross-server window end-to-end. The `Reserve() (port, listener)`
  signature from #173 is the baseline for step 7's port allocator
  interface.

The dependency on phase 3-5 is the largest: roughly half of phase 3-8's
work happens inside `internal/orchestrator/` rebuilding the package so
both backend variants share infrastructure.

## Deliverables

1. **Backend selection via build tags** — refactor backend construction
   to a factory map registered from `init()` in tag-gated files. Default
   `go build` includes all backends; variant builds opt in via
   `-tags 'minimal,docker_backend'` (or `process_backend`, or future
   `kubernetes_backend`). The Docker SDK and process backend code each
   enter the dependency graph only via their respective tag-gated
   wrapper files. A binary built with `minimal,process_backend` does
   not import `github.com/moby/moby/client` and cannot talk to a
   Docker socket even if one is mounted.

2. **Outer-container seccomp profile** (`docker/blockyard-seccomp.json`)
   — JSON profile based on Docker's default that allows
   `clone`/`unshare` with `CLONE_NEWUSER` without `CAP_SYS_ADMIN`.
   Must-have artifact: without it, Docker's default profile blocks
   bwrap's `--unshare-user` inside the outer container with `EPERM`
   and the containerized process backend is broken out of the box.
   Operators pass it to the outer container via `--security-opt
   seccomp=blockyard-seccomp.json`. No other isolation properties
   are relaxed.

3. **Bwrap seccomp profile + build step**
   (`docker/blockyard-bwrap-seccomp.json`, `cmd/seccomp-compile/`) —
   separate JSON profile applied to the worker R process *inside*
   the bwrap sandbox via bwrap's `--seccomp <fd>` flag. Bwrap expects
   pre-compiled BPF binary, not JSON, so phase 3-8 ships a small Go
   program that reads the JSON at build time and emits the BPF blob
   using `libseccomp-golang`. The compiled blob is shipped at
   `/etc/blockyard/seccomp.bpf` in the `blockyard-process` and
   `blockyard` images; `process.seccomp_profile` defaults to that
   path via an env var set in the image.

4. **Variant Docker images**
   - `ghcr.io/cynkra/blockyard:<v>` — full image, all backends
     compiled, R + bwrap + ca-certificates + iptables installed.
   - `ghcr.io/cynkra/blockyard-docker:<v>` — slim image, Docker
     backend only. Same content as today's `blockyard:<v>`.
   - `ghcr.io/cynkra/blockyard-process:<v>` — process-backend
     image, R + bwrap + bwrap seccomp profile + binary. No Docker
     SDK in the binary, no socket expectation.

5. **CI workflow expansion** — the `server.yml` matrix expands from
   2 entries (amd64 + arm64 of one image) to 6 entries (3 variants
   × 2 architectures). `release.yml` publishes per-variant multi-arch
   manifests and runs Trivy per variant.

6. **Process orchestrator variant**
   (`internal/orchestrator/clone_process.go`) — fork+exec implementation
   of the "create a new server instance" step. The shared cutover code
   (drain, activate, watchdog, scheduled, rollback) moves into
   backend-agnostic files and uses a small `serverFactory` interface
   with two implementations: Docker container clone (existing, moved)
   and process fork+exec (new). The process variant binds the new
   blockyard to an alternate port from a configurable range, uses the
   same activation token mechanism, and exits the old server after
   its session count reaches zero or a configurable drain timeout
   elapses.

7. **`update.alt_bind_range` config field** — the port range from
   which the process orchestrator picks an alternate bind for the new
   server. Operator-configured, separate from `[process] port_range`
   (worker pool). Defaults to `"8090-8099"`.

8. **Redis-backed resource allocators** for ports and UIDs
   (`internal/backend/process/ports.go`, `.../uids.go`) — refactor
   both allocators into interfaces with two implementations each:
   the current in-memory bitset (used when Redis is not configured)
   and new Redis-backed allocators (used when Redis is configured).
   Closes two cross-server collision windows surfaced by the rolling-
   update cutover: (a) UIDs are guaranteed to collide without shared
   state since the kernel offers no probe mechanism, (b) ports can
   still collide after #173's pre-fork tightening because the "probe
   succeeded → fork → child bind" window is seconds-long during R
   startup, giving another peer time to probe the same port. Both
   share the same pattern: Lua-scripted SETNX scan, one-key-per-
   resource with hostname ownership, Lua-scripted ownership-checked
   Release, `CleanupOrphanResources` for crash recovery. The port
   allocator adds a kernel-probe retry loop on top of the shared
   pattern because Redis only coordinates among blockyard peers,
   not among all host processes.

9. **Native and containerized deployment guides**
   (`docs/src/content/docs/guides/process-backend.md`,
   `.../process-backend-container.md`) — operator docs: distro
   prerequisites, egress firewall rules, system user creation, systemd
   unit, reverse proxy setup for rolling updates, limitations, and
   (containerized) the seccomp profile extraction workflow.
   `docs/design/backends.md` gains a short rolling-update section
   cross-linking the guides.

10. **Tests** — build-tag wiring, dependency-graph exclusion check,
    seccomp compilation round-trip, process rolling update end-to-end
    integration (against real Redis), and variant-image smoke tests
    in CI.

## Step-by-step

### Step 1: Backend selection via build tags

The current `cmd/blockyard/main.go` directly imports both backend
packages and selects via a switch on `cfg.Server.Backend`. Phase 3-8
replaces the direct imports with a factory map populated via `init()`
in tag-gated files.

#### Build tag scheme

One mode tag plus one opt-in tag per backend:

| Tag | Purpose |
|---|---|
| `minimal` | Switch from default-include-all to opt-in mode. |
| `docker_backend` | Include the Docker backend (under `minimal`). |
| `process_backend` | Include the process backend (under `minimal`). |
| `kubernetes_backend` | Reserved for v4. Same shape. |

File-level expression on each tag-gated file:

```go
//go:build !minimal || docker_backend
```

"Compile unless we're in minimal mode and docker_backend was not
requested." Default `go build` with no tags sees `!minimal` as true
and includes everything. `go build -tags 'minimal,docker_backend'`
has the first half false and the second half true — still included.
`go build -tags 'minimal,process_backend'` excludes the docker-tagged
files.

Build invocations:

| Variant | Command |
|---|---|
| Full image (default) | `go build` |
| Docker backend only | `go build -tags 'minimal,docker_backend'` |
| Process backend only | `go build -tags 'minimal,process_backend'` |
| k8s only (future) | `go build -tags 'minimal,kubernetes_backend'` |

Adding a new backend later requires no changes to existing files —
just create new tag-gated files with
`//go:build !minimal || <new>_backend`.

#### Factory pattern

`cmd/blockyard/main.go` becomes backend-agnostic:

```go
var backendFactories = map[string]backendFactory{}

// version is the third arg because docker.New needs it (for the orchestrator's
// version-comparison/release-check path) and threading it through the factory
// type is cleaner than capturing it from a package-level var that init() can
// only read after main() has assigned it.
type backendFactory func(ctx context.Context, cfg *config.Config, version string) (backend.Backend, error)

func main() {
    // ...load config, etc...
    factory, ok := backendFactories[cfg.Server.Backend]
    if !ok {
        slog.Error("backend not available in this build",
            "backend", cfg.Server.Backend,
            "available", availableBackends())
        os.Exit(1)
    }
    be, err := factory(ctx, cfg, version)
    // ...rest of main unchanged...
}
```

The error message includes the list of backends actually compiled
into this binary so a misconfigured operator sees "backend 'docker'
not available in this build (available: process)" rather than a
cryptic import failure.

Each backend wrapper file registers its factory in `init()`:

```go
// cmd/blockyard/backend_docker.go
//go:build !minimal || docker_backend
package main

import (
    "github.com/cynkra/blockyard/internal/backend/docker"
    // ...
)

func init() {
    backendFactories["docker"] = func(ctx context.Context, cfg *config.Config, version string) (backend.Backend, error) {
        return docker.New(ctx, cfg, cfg.Storage.BundleServerPath, version)
    }
}
```

`backend_process.go` mirrors this with `process.New(cfg)` and ignores
the `version` arg (the process backend has no equivalent of pulling a
prior image, so it doesn't need to know its own version). When the
build excludes one of these files, the corresponding backend package
is never imported and Go's dep graph never pulls it in.

#### Orchestrator wrapper files

The same scheme applies to `internal/orchestrator/`. Today `clone.go`
and `helpers.go` import `github.com/moby/moby/client`. Phase 3-8
splits along that seam:

- `internal/orchestrator/orchestrator.go` — backend-agnostic
  `Orchestrator` struct, `Update()`, `Watchdog()`, `Rollback()` core
  flow, state management. No moby imports.
- `internal/orchestrator/helpers.go` — `waitReady`, `activate`,
  `checkReady`, `generateActivationToken`, `listenPort`. HTTP-level,
  backend-agnostic. The Docker-specific helpers (`pullImage`,
  `containerAddr`, `killAndRemove`, `currentImageBase/Tag`) move out.
  `waitReady`'s signature changes from `(ctx, containerID) (addr, err)`
  to `(ctx, addr string) error` — no more container-address lookup
  inside the poll loop, because `CreateInstance` has already
  resolved the address before returning. The Docker-specific
  inspect-retry that used to live in `waitReady` moves into
  `dockerServerFactory.CreateInstance`, bounded by the same ctx
  deadline the orchestrator sets from `cfg.Proxy.WorkerStartTimeout`.
- `internal/orchestrator/serverfactory.go` (new, untagged) — defines
  the `ServerFactory` and `newServerInstance` interfaces the core
  uses to delegate "create a new server instance":

  ```go
  type ServerFactory interface {
      // CreateInstance starts the new server instance and blocks until
      // its address is resolvable. On success, the returned instance's
      // Addr() is immediately usable for polling and activation — no
      // async resolution, no retry loop required by the caller. The
      // ctx's deadline (set by the orchestrator from
      // cfg.Proxy.WorkerStartTimeout) bounds address resolution; the
      // remaining budget flows through to waitReady for /readyz polling.
      CreateInstance(ctx context.Context, ref string, extraEnv []string, sender task.Sender) (newServerInstance, error)
      PreUpdate(ctx context.Context, version string, sender task.Sender) error
  }

  type newServerInstance interface {
      ID() string            // stable identifier for logging
      Addr() string          // host:port; cheap, synchronous, cached at CreateInstance time
      Kill(ctx context.Context) // tear down on failure or watchdog rollback
  }
  ```

  `Addr()` is a cached synchronous getter because both variants can
  cache the address at creation time: the process factory already
  knows the alt-bind port before `cmd.Start`, and the Docker factory
  runs its own inspect-retry loop inside `CreateInstance` until the
  container's IP appears in `NetworkSettings.Networks`, only then
  returning the instance. This keeps `waitReady` a pure /readyz
  poller and means `o.activeInstance.Addr()` calls from Update and
  Watchdog (see the collapsed hand-off in step 6) need no context
  or error handling.

- `internal/orchestrator/clone_docker.go` (new, tagged) — Docker
  implementation: `dockerServerFactory`, `dockerInstance`,
  `cloneConfig`, `startClone`, `containerAddr`, image pull, kill.
  Most of the existing `clone.go`/`helpers.go` Docker bits move
  here.
- `internal/orchestrator/clone_process.go` (new, tagged) —
  fork+exec implementation, see step 6.

#### Wiring the factory from main

When the everything variant is built, both backend wrapper files
compile into the same package. They cannot define a top-level
function with the same name, so wiring uses a slice populated from
`init()`:

```go
// cmd/blockyard/orchestrator.go (untagged)

var orchestratorFactoryFns []func(*server.Server, *config.Config, backend.Backend) orchestrator.ServerFactory

func newServerFactory(srv *server.Server, cfg *config.Config, be backend.Backend) orchestrator.ServerFactory {
    for _, fn := range orchestratorFactoryFns {
        if f := fn(srv, cfg, be); f != nil {
            return f
        }
    }
    return nil // no orchestrator available — admin endpoints return 501
}
```

```go
// cmd/blockyard/orchestrator_docker.go
//go:build !minimal || docker_backend

func init() {
    orchestratorFactoryFns = append(orchestratorFactoryFns,
        func(srv *server.Server, cfg *config.Config, be backend.Backend) orchestrator.ServerFactory {
            if dbe, ok := be.(*docker.DockerBackend); ok && dbe.ServerID() != "" {
                return orchestrator.NewDockerFactory(dbe.Client(), dbe.ServerID())
            }
            return nil
        })
}
```

`orchestrator_process.go` mirrors this, checking
`be.(*process.ProcessBackend)`. Each candidate returns nil unless
its backend matches, so the order of slice evaluation is irrelevant.

The orchestrator package itself never imports the backend packages;
wiring lives entirely in `cmd/blockyard/`. This keeps the
orchestrator package buildable in any variant, even when neither
backend is included (for package-level tests).

#### Verification

Two layers of tests catch regressions:

- **Per-variant factory tests** in `cmd/blockyard/build_tags_*_test.go`
  (three files, each with a different `//go:build` tag set) verify
  the registered factory set matches the expected backends for that
  variant.
- **Dependency-graph test** in `internal/build/deps_test.go` invokes
  `go list -deps -tags 'minimal,process_backend' ./cmd/blockyard`
  and asserts the output does not contain `github.com/moby/moby` or
  `internal/backend/docker`. Symmetric test for the docker variant.
  Catches regressions where a future change adds an untagged import
  that pulls a backend into the wrong variant.

### Step 2: Outer-container seccomp profile (JSON)

Docker's default seccomp profile blocks `clone()` and `unshare()`
calls that include the `CLONE_NEWUSER` flag unless the process has
`CAP_SYS_ADMIN`. The relevant upstream rule:

```json
{
    "names": ["clone", "unshare", "..."],
    "action": "SCMP_ACT_ALLOW",
    "includes": {"caps": ["CAP_SYS_ADMIN"]}
}
```

Without `CAP_SYS_ADMIN`, these syscalls return `EPERM`. When bwrap
inside an outer Docker container calls `unshare(CLONE_NEWUSER)`, the
kernel checks the outer container's seccomp filter, sees the process
lacks `CAP_SYS_ADMIN`, and blocks the call. Bwrap exits with an
error and the worker fails to spawn. The containerized process
backend is unusable out of the box.

The fix is a custom seccomp profile identical to Docker's default
in every respect except: it adds an unconditional allow rule for
`clone`, `clone3`, `unshare`, and `setns` (placed before the
cap-gated entry, since seccomp evaluates rules in order). No other
capability gates are relaxed, no additional syscalls are added, and
the existing cap-restricted entries for other syscalls stay intact.

#### Vendored upstream + overlay

The upstream Docker seccomp profile evolves between Docker releases.
To keep the blockyard profile in sync, phase 3-8 adopts a vendor +
overlay pattern:

- `docker/upstream-default-seccomp.json` — vendored copy of moby's
  `default.json` for the version we depend on. Regenerated when
  `go.mod` bumps moby.
- `docker/blockyard-seccomp-overlay.json` — hand-edited file
  containing only the blockyard-specific additions (~20 lines).
- `docker/blockyard-seccomp.json` — merged output, committed to the
  repo and shipped in the images.
- `cmd/seccomp-merge/main.go` — ~80-line Go program (no CGO) that
  reads the upstream and overlay files and emits the merged JSON.
- `make regen-seccomp` — invokes `seccomp-merge` after copying the
  current moby profile from `$GOPATH/pkg/mod`.

CI runs `make regen-seccomp` and fails if the result differs from
the committed file, catching drift when moby is bumped.

#### Distribution to operators

Docker's `--security-opt seccomp=...` reads the profile from the
host, not from inside the container. Operators need the profile on
disk before the container starts. Two paths:

1. **`by admin install-seccomp [--target /path]`** — new CLI
   subcommand that writes the profile. The JSON is embedded into
   the `by` binary at build time via `//go:embed`. CI verifies the
   embedded copy matches the on-disk source.
2. **Direct download** from the GitHub release — the release
   workflow uploads `blockyard-seccomp.json` as a release asset.

#### Compose example

```yaml
services:
  blockyard:
    image: ghcr.io/cynkra/blockyard-process:1.2.3
    security_opt:
      - seccomp=/etc/blockyard/seccomp.json
    volumes:
      - blockyard-data:/data
    environment:
      - BLOCKYARD_REDIS_URL=redis://redis:6379
    networks: [state, default]
    ports: ["8080:8080"]
```

No `--privileged`, no `cap_add`, no Docker socket bind mount.

### Step 3: Bwrap seccomp profile (JSON + BPF compile step)

The outer-container profile from step 2 has no effect on bwrap's
inner sandbox. Bwrap supports its own seccomp filter via the
`--seccomp <fd>` flag (see phase 3-7 step 3), which applies a
separate BPF program to the worker R process *inside* the namespace.
Phase 3-7 left `SeccompProfile = ""` so no inner filter was applied;
phase 3-8 ships the profile and turns the filter on.

#### Profile authoring

The bwrap profile is *also* derived from Docker's default — it's
appropriate for any unprivileged process running untrusted code,
and the worker R processes match that description. Two key
differences from the outer profile:

- **Stricter on namespace creation**: the bwrap profile re-tightens
  `clone`/`unshare` that the outer profile relaxed. Workers should
  not be creating further namespaces once inside the sandbox.
- **Drops a few more syscalls**: `mount`, `umount`, `pivot_root`,
  `chroot`, `swapon`, `swapoff`, `reboot`, `kexec_load`,
  `init_module`. These are already blocked by Docker's default so
  the bwrap profile is at most as strict as the outer, plus the
  namespace re-tightening.

Profile source: `docker/blockyard-bwrap-seccomp.json`. Same
vendored-upstream + overlay pattern as the outer profile, with its
own overlay file containing the blockyard-specific additions for the
bwrap variant.

#### JSON → BPF compilation

Bwrap's `--seccomp <fd>` expects an already-compiled BPF binary blob
and calls `prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, <bpf>)` with
the contents. JSON is not accepted; compilation is the build-time
orchestration step.

`cmd/seccomp-compile/main.go` is a ~120-line Go program that:

1. Reads an OCI seccomp JSON profile (subset of the schema — default
   action, syscall names, action, argument matchers with operators
   like `SCMP_CMP_MASKED_EQ`).
2. Builds an in-memory filter via `github.com/seccomp/libseccomp-golang`
   — `libseccomp.NewFilter(defaultAct)`, `filter.AddRule` or
   `filter.AddRuleConditional` for each syscall entry, with action
   and operator mapping from the JSON strings to libseccomp constants.
3. Unknown syscalls (arch-specific) are skipped silently, matching
   libseccomp's own runtime behavior.
4. Capability gating (`includes.caps`) in the input is flattened to
   unconditional allow — the build environment always has the cap,
   so the merged rule is equivalent.
5. Exports the BPF binary via `filter.ExportBPF(file)`.

The dependency: `github.com/seccomp/libseccomp-golang`, which
requires **CGO** and the system `libseccomp-dev` package at build
time. This is the new build dependency phase 3-8 introduces. The
`seccomp-compile` binary itself is never shipped to operators — it
runs only in a build stage that produces the BPF blob, and the
blockyard runtime binary stays CGO-disabled. Pure-Go alternatives
exist but are less mature; reimplementing OCI profile parsing
against libseccomp's hardened C is not worth the risk.

#### Build pipeline

The BPF blob is produced in two places:

- **In the variant Dockerfiles** — a `seccomp-compiler` stage runs
  `seccomp-compile` and `COPY`s the output into the final stage at
  `/etc/blockyard/seccomp.bpf`. The Dockerfile sets
  `ENV BLOCKYARD_PROCESS_SECCOMP_PROFILE=/etc/blockyard/seccomp.bpf`
  so the default `process.seccomp_profile` is correct in the image
  without TOML changes.
- **In the release workflow** — a `seccomp-blob` job runs
  `seccomp-compile` against the committed JSON and uploads the
  resulting `.bpf` as a release asset. Native deployments fetch it
  and point `process.seccomp_profile` at the local path.

#### Validation

Phase 3-8 adds `checkSeccompProfile` to the process backend's
preflight: opens the configured profile, verifies it's a readable
file with a BPF-program shape. If the file exists but is malformed
or unreadable, the server fails fast at startup rather than at
first worker spawn.

### Step 4: Variant Docker images

Three Dockerfiles, three published images, sharing the early build
stages (docs, css-builder, builder, seccomp-compiler).

- **`docker/server.Dockerfile`** stays the slim docker-backend
  image. Current content is kept; only the `go build` invocation
  gains `-tags 'minimal,docker_backend'`. Output image:
  `ghcr.io/cynkra/blockyard-docker:<v>`.
- **`docker/server-process.Dockerfile`** (new) produces the
  process-backend image. Based on `ghcr.io/rocker-org/r-ver:4.4.3`
  (see rationale below). Installs `bubblewrap`, `ca-certificates`,
  `curl` via apt. Copies the `blockyard` binary built with
  `-tags 'minimal,process_backend'`, the compiled BPF blob from
  the `seccomp-compiler` stage, and `docker/blockyard-seccomp.json`
  (shipped so operators can extract it via `docker run ... cat`).
  Sets `ENV BLOCKYARD_PROCESS_SECCOMP_PROFILE=/etc/blockyard/seccomp.bpf`.
  No `iptables`, no Docker SDK in the binary.
- **`docker/server-everything.Dockerfile`** (new) is essentially
  `server-process.Dockerfile` + `iptables` in apt-get + no build
  tags on `go build` (default includes both backends). Output
  image: `ghcr.io/cynkra/blockyard:<v>`. Base is also
  `ghcr.io/rocker-org/r-ver:4.4.3` since R is the expensive
  dependency and including it makes the `iptables` tooling cheap
  by comparison.

Key `seccomp-compiler` stage (shared by process and everything
variants):

```dockerfile
FROM golang:1.25.9-alpine AS seccomp-compiler
RUN apk add --no-cache build-base libseccomp-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/seccomp-compile/ cmd/seccomp-compile/
COPY docker/blockyard-bwrap-seccomp.json /tmp/bwrap-seccomp.json
RUN CGO_ENABLED=1 go build -o /seccomp-compile ./cmd/seccomp-compile && \
    /seccomp-compile -in /tmp/bwrap-seccomp.json -out /blockyard-bwrap-seccomp.bpf
```

CGO is active only in this stage; the runtime binary build stage
stays `CGO_ENABLED=0`.

**Base image choice — rocker/r-ver, not Debian + manual R.** Rocker
maintains R-on-Linux images with the right system libraries for
common R packages, `R_LIBS` paths, `LANG`/`LC_ALL` for R numerics.
Reproducing this from scratch on Debian is fragile across R versions
and package dependencies, and the marginal size saving isn't worth
the maintenance burden. Alpine + R is not viable — R on musl has
known numerics and locale issues, and many R packages fail to build
against musl.

The pinned tag `ghcr.io/rocker-org/r-ver:4.4.3` matches what phase
3-7's CI already uses (`.github/workflows/ci.yml` process-backend
matrix runs inside the same image) and what `blockyard.toml` ships
as the default worker image. Dockerfile, CI, and default config all
reference the GHCR mirror (not Docker Hub's `rocker/r-ver`) to
avoid Docker Hub anonymous-pull rate limits and keep a single
source of truth for R versions. Test environment and runtime image
must agree on the R version — drift between them is a silent
reproducibility hazard for R package builds.

**Three Dockerfiles, not one with `ARG` switches.** Dockerfile
conditionals (via `ARG`-driven shell tricks) make the build harder
to read and harder to cache predictably with buildx. Three explicit
files have visible duplication in the early stages but auditable
structure. A small CI check diffs the early stages and flags drift
if it matters.

### Step 5: CI workflow expansion

`.github/workflows/server.yml` expands from 2 entries to 6 (3
variants × 2 architectures). A flat matrix with per-entry
`dockerfile`, `image_suffix`, `runner`, `platform` keeps the cross
product readable:

```yaml
strategy:
  fail-fast: false
  matrix:
    include:
      - { variant: docker,     dockerfile: docker/server.Dockerfile,            image_suffix: -docker,  runner: ubuntu-24.04,     platform: linux/amd64 }
      - { variant: docker,     dockerfile: docker/server.Dockerfile,            image_suffix: -docker,  runner: ubuntu-24.04-arm, platform: linux/arm64 }
      - { variant: process,    dockerfile: docker/server-process.Dockerfile,    image_suffix: -process, runner: ubuntu-24.04,     platform: linux/amd64 }
      - { variant: process,    dockerfile: docker/server-process.Dockerfile,    image_suffix: -process, runner: ubuntu-24.04-arm, platform: linux/arm64 }
      - { variant: everything, dockerfile: docker/server-everything.Dockerfile, image_suffix: "",       runner: ubuntu-24.04,     platform: linux/amd64 }
      - { variant: everything, dockerfile: docker/server-everything.Dockerfile, image_suffix: "",       runner: ubuntu-24.04-arm, platform: linux/arm64 }
```

Build output tags use `${{ env.IMAGE }}${{ matrix.image_suffix }}:build-${{ platform-slug }}`.

`release.yml` gains per-variant Trivy scans (amd64 only, to bound CI
time — arch-specific CVE delta is typically zero) and per-variant
`docker buildx imagetools create` invocations to publish the
multi-arch manifests under the three image names.

The `binaries` job is unchanged. The `blockyard` server binary is
still built for linux/amd64 + linux/arm64 only; process backend is
Linux-only and no darwin server binaries are added (operators who
want blockyard on a Mac use the Docker backend image via Docker
Desktop).

A new `server-smoke` job runs after `server-image` and pulls each
variant's amd64 image, runs it, and checks `/healthz`:

```yaml
server-smoke:
  needs: server-image
  strategy:
    matrix:
      variant: ["", "-docker", "-process"]
  steps:
    - name: Pull and smoke-test
      run: |
        IMAGE="${{ env.SERVER_IMAGE }}${{ matrix.variant }}:build-linux-amd64"
        docker pull "$IMAGE"
        SECCOMP=""
        if [ "${{ matrix.variant }}" = "-process" ]; then
          # Override the entrypoint — the image's default entrypoint is
          # ["blockyard", "--config", ...], so without --entrypoint the
          # `cat` ends up as an arg to blockyard.
          docker run --rm --entrypoint cat "$IMAGE" /etc/blockyard/seccomp.json > /tmp/seccomp.json
          SECCOMP="--security-opt seccomp=/tmp/seccomp.json"
        fi
        docker run -d --name smoke $SECCOMP -p 18080:8080 "$IMAGE"
        for i in {1..30}; do
          if curl -sf http://localhost:18080/healthz >/dev/null; then
            docker rm -f smoke; exit 0
          fi
          sleep 1
        done
        docker logs smoke; docker rm -f smoke; exit 1
```

Catches packaging-level regressions (bad Dockerfile, broken
entrypoint, incompatible seccomp profile path) against the actual
published artifact.

#### `:latest` rename consequence

Today `ghcr.io/cynkra/blockyard:latest` is the slim Docker-only
image. Under the three-image scheme it becomes the **everything**
variant. Existing operators pulling `:latest` will get a larger
image (~5–10× size due to R) but deployments keep working — the
everything image supports the Docker backend transparently.
Operators wanting the slim image switch to
`ghcr.io/cynkra/blockyard-docker:latest`. Operators pinning a
version are unaffected until they bump.

The release notes for the version shipping phase 3-8 call this out
with a sed command for compose files:

```
sed -i 's|cynkra/blockyard:latest|cynkra/blockyard-docker:latest|g' docker-compose.yml
```

This is the cost of symmetric three-name naming; the alternative
(keeping `blockyard:<v>` as docker-only and naming the everything
variant asymmetrically) would avoid the breakage at the cost of
confusing naming.

### Step 6: Process orchestrator (fork+exec parallel cutover)

The largest implementation chunk after the build-tag refactor. The
process orchestrator creates a new server instance by fork+execing
the same blockyard binary with `BLOCKYARD_PASSIVE=1` and an alternate
bind port, then runs the same cutover/watchdog/scheduled flow as the
Docker variant.

#### Flow

1. `by admin update` triggers `Orchestrator.Update(ctx, channel, sender)`.
2. `Update` calls `factory.PreUpdate` (variant-specific: docker pulls
   the new image, process just backs up the DB).
3. `Update` generates an activation token, derives a ctx with
   `cfg.Proxy.WorkerStartTimeout` as its deadline, and calls
   `factory.CreateInstance(startCtx, version, []string{...}, sender)`.
   For the process variant, this picks a free port from
   `[update] alt_bind_range`, resolves `executableFn()`, and
   `cmd.Start()`s a new blockyard child with an env containing
   `BLOCKYARD_PASSIVE=1`, `BLOCKYARD_SERVER_BIND=0.0.0.0:<altport>`,
   and `BLOCKYARD_ACTIVATION_TOKEN=<token>`. Everything else from the
   old server's env is copied. `Setsid: true`, no `Pdeathsig`. The
   Docker variant blocks inside `CreateInstance` running its inspect
   loop against the same `startCtx` until the container's IP lands in
   `NetworkSettings.Networks`, then returns with `Addr()` populated.
   The remaining `startCtx` budget flows into the next step.

   The bind override goes through `applyEnvOverrides` (the reflective
   `BLOCKYARD_<SECTION>_<FIELD>` walker in `internal/config/config.go`),
   so the env var name must match the toml path exactly —
   `BLOCKYARD_SERVER_BIND`, not `BLOCKYARD_BIND`. `BLOCKYARD_PASSIVE`
   and `BLOCKYARD_ACTIVATION_TOKEN` are special direct env vars
   (read via `os.Getenv` in `main.go` and `internal/api/admin.go`)
   and stay as-is.
4. `waitReady(startCtx, inst.Addr())` polls `/readyz` on the new
   instance's addr (via loopback, `127.0.0.1:<altport>`, for the
   process variant; container IP for Docker) until 200. The same
   `startCtx` bounds both this poll and the preceding `CreateInstance`,
   so the total "new-server-becomes-healthy" budget remains
   `cfg.Proxy.WorkerStartTimeout` — matching today's single-budget
   semantics rather than splitting into two independent timeouts.
5. `drainFn()` on the old server (health → 503). The operator's
   reverse proxy stops routing new traffic to the old port.
6. `activate(ctx, newAddr)` posts to `/admin/activate` on the new
   instance with the activation token.
7. The orchestrator enters watchdog mode. When the watch period
   elapses and the new instance is healthy, `runScheduledOnce`
   signals `exitFn()`, which wakes the main goroutine's `doneCh`
   select. Main calls `drainer.Finish` — and because
   `Drainer.FinishIdleWait` is set to
   `cfg.Update.DrainIdleWait.Duration` on the process backend
   (default 5 minutes), `Finish` first polls the old server's
   local session count until it reaches zero (or the timeout
   elapses) and only then proceeds with the normal teardown.
8. The new server, being a child of the old server but *without*
   `Pdeathsig`, survives the old's exit. Its parent becomes
   init/systemd. The new server's autoscaler rebuilds the worker
   pool from new traffic.

#### Alt bind range + idle-wait config

Two new fields in `UpdateConfig`:

```go
type UpdateConfig struct {
    Schedule      string   `toml:"schedule"`
    Channel       string   `toml:"channel"`
    WatchPeriod   Duration `toml:"watch_period"`
    AltBindRange  string   `toml:"alt_bind_range"`  // e.g. "8090-8099"
    DrainIdleWait Duration `toml:"drain_idle_wait"` // max time to wait for sessions before teardown
}
```

`AltBindRange` defaults to `"8090-8099"` in `applyDefaults()`.
Parsing and free-port selection go through a new shared helper
`internal/units/portrange.go` (used by both the worker port range
and the alt bind range).

`DrainIdleWait` defaults to `5 * time.Minute` — same as
`WatchPeriod`'s default and a reasonable ceiling for "most
interactive sessions finish naturally while the operator's
rolling update is in progress." `updateDefaults()` populates the
default when the field is unset. Only the process backend reads
this field (via `finishIdleWaitForBackend`); the Docker backend
cuts over hard and relies on the reverse proxy to drain in-flight
requests, so the field is ignored there.

No explicit "disable" semantic — the process backend needs a
non-zero idle-wait because workers are killed by `Pdeathsig` when
the old server exits, and sessions on those workers end abruptly
unless the idle-wait lets them finish first. Operators who want a
faster cutover set `drain_idle_wait = "10s"` (or similar); the
floor is the 5-second poll interval inside `waitForIdle`.

The orchestrator picks a free port by calling `net.Listen` and
closing immediately. TOCTOU window is small but non-zero — if the
port is taken between probe and the new server's actual bind, the
new server fails with "address already in use" and the orchestrator
retries the next port in the range.

Separate from `[process] port_range` (worker pool) by design: during
the overlap window both servers allocate workers from the same worker
range, and borrowing the alt bind from that pool would reduce
worker capacity at exactly the wrong moment.

#### `processServerFactory` sketch

```go
//go:build !minimal || process_backend
package orchestrator

type processServerFactory struct {
    cfg *config.Config
}

func NewProcessFactory(cfg *config.Config) ServerFactory {
    return &processServerFactory{cfg: cfg}
}

func (f *processServerFactory) CreateInstance(
    ctx context.Context,
    _ string, // ref unused — process variant always execs the same binary
    extraEnv []string,
    sender task.Sender,
) (newServerInstance, error) {
    altBind, err := f.pickAltBind(nil)
    if err != nil {
        return nil, fmt.Errorf("pick alt bind: %w", err)
    }
    self, err := os.Executable()
    if err != nil {
        return nil, fmt.Errorf("resolve own executable: %w", err)
    }
    env := os.Environ()
    env = setEnv(env, "BLOCKYARD_PASSIVE", "1")
    env = setEnv(env, "BLOCKYARD_SERVER_BIND", altBind)
    for _, kv := range extraEnv {
        if k, v, ok := strings.Cut(kv, "="); ok {
            env = setEnv(env, k, v)
        }
    }
    // strip systemd-propagated vars that should not carry over
    env = stripEnv(env, "INVOCATION_ID", "JOURNAL_STREAM")

    argv := []string{self}
    if f.cfg.ConfigPath != "" {
        argv = append(argv, "--config", f.cfg.ConfigPath)
    }

    cmd := exec.Command(argv[0], argv[1:]...)
    cmd.Env = env
    cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
    cmd.SysProcAttr = &syscall.SysProcAttr{
        Setsid: true, // detach from old server's pgrp
        // No Pdeathsig — child must outlive parent.
    }

    if err := cmd.Start(); err != nil {
        return nil, fmt.Errorf("start new blockyard: %w", err)
    }
    go func() { _ = cmd.Wait() }() // reap to avoid zombie

    return &processInstance{pid: cmd.Process.Pid, addr: altBind, cmd: cmd}, nil
}

func (p *processInstance) Addr() string {
    // Rewrite 0.0.0.0:<port> to 127.0.0.1:<port> for loopback polling.
    if strings.HasPrefix(p.addr, "0.0.0.0:") {
        return "127.0.0.1:" + strings.TrimPrefix(p.addr, "0.0.0.0:")
    }
    return p.addr
}

func (p *processInstance) Kill(ctx context.Context) {
    if p.cmd.Process == nil {
        return
    }
    _ = p.cmd.Process.Signal(syscall.SIGTERM)
    done := make(chan struct{})
    go func() { _ = p.cmd.Wait(); close(done) }()
    select {
    case <-done:
    case <-time.After(10 * time.Second):
        _ = p.cmd.Process.Kill()
    case <-ctx.Done():
        _ = p.cmd.Process.Kill()
    }
}
```

The `setEnv`/`stripEnv` helpers are small idempotent operations on
`[]string` KEY=VALUE slices.

#### Collapsed `Update`/`Watchdog` hand-off

Today's `Orchestrator.Update` returns a `*UpdateResult{ContainerID,
Addr}` that the admin handler extracts and passes to `Watchdog`:

```go
ur, err := orch.Update(bgCtx, channel, sender)
// ...
orch.Watchdog(bgCtx, ur.ContainerID, ur.Addr, watchPeriod, sender)
```

With the `newServerInstance` interface landing, `ContainerID` is
no longer meaningful (the process factory has no container IDs —
it has a PID and a `Kill` closure). Rather than expose the
interface through `UpdateResult` and leak it into the API layer,
phase 3-8 collapses the hand-off: the orchestrator holds the
active instance in a private field, set during `Update` and
consumed during `Watchdog` / `Rollback`.

```go
type Orchestrator struct {
    // ...existing fields...
    factory        ServerFactory
    activeInstance newServerInstance // set by Update, read by Watchdog/Rollback
}

func (o *Orchestrator) Update(ctx context.Context, channel string, sender task.Sender) (bool, error) {
    // ... check for update, PreUpdate, backup, CreateInstance ...
    inst, err := o.factory.CreateInstance(ctx, ref, []string{...}, sender)
    // ...
    o.activeInstance = inst
    // ... waitReady(ctx, inst.Addr()), drainFn, activate(ctx, inst.Addr()) ...
    return true, nil
}

func (o *Orchestrator) Watchdog(ctx context.Context, watchPeriod time.Duration, sender task.Sender) error {
    defer func() { o.activeInstance = nil }()
    addr := o.activeInstance.Addr()
    for {
        // ... poll /readyz on addr ...
        // on failure:
        o.activeInstance.Kill(ctx)
        o.undrainFn()
        return err
    }
}
```

Admin handler becomes:

```go
updated, err := orch.Update(bgCtx, channel, sender)
if err != nil { /* ... */ }
if !updated {
    sender.Complete(task.Completed)
    orch.SetState("idle")
    return
}
watchPeriod := /* ... */
if err := orch.Watchdog(bgCtx, watchPeriod, sender); err != nil { /* ... */ }
```

No `UpdateResult` type, no hand-off of opaque instance data
through the admin goroutine. The orchestrator's state machine
(`idle` → `updating` → `watching` → `idle`) already serializes
`Update` → `Watchdog` transitions via `CASState`, so the
`activeInstance` field is only ever read between those phases by
one caller — no additional locking needed beyond the existing
state machine discipline.

`Rollback` follows the same pattern: `CreateInstance` → stash on
`activeInstance` → waitReady/drain/activate → clear on return.
Rollback has no watchdog, so the field lives for the duration of
one `Rollback` call.

#### `Config.ConfigPath`

The factory needs the config file path so the new blockyard reads
the same TOML. `main.go` stores it programmatically:

```go
cfg, err := config.Load(*configPath)
// ...
cfg.ConfigPath = *configPath // not part of TOML, populated at startup
```

`Config.ConfigPath` is a new untaggable field (no `toml:` tag, not
validated, no default).

#### `Drainer.Finish` gains an idle-wait prelude

Phase 3-4's `Finish()` shuts down HTTP listeners immediately and
severs hijacked WebSocket connections. The process orchestrator
needs softer behavior: wait until sessions end naturally, then run
the existing teardown. Adding a separate `FinishWhenIdle` public
method would force every call site to know which variant it's in;
instead, `Finish` gains a pre-teardown idle-wait that's controlled
by a new field on the `Drainer` struct:

```go
type Drainer struct {
    Srv             *server.Server
    MainServer      *http.Server
    MgmtServer      *http.Server
    BGCancel        context.CancelFunc
    BGWait          *sync.WaitGroup
    TracingShutdown func(context.Context) error

    // FinishIdleWait, if non-zero, makes Finish wait up to this
    // duration for the local server's session count to reach zero
    // before tearing down. Set by main.go for the process backend;
    // zero for docker (which cuts over hard and relies on the
    // reverse proxy to drain the last requests).
    FinishIdleWait time.Duration
}

func (d *Drainer) Finish(timeout time.Duration) {
    if d.FinishIdleWait > 0 {
        d.waitForIdle(d.FinishIdleWait)
    }
    slog.Info("finish: shutting down (workers survive)")
    // ...existing Finish body unchanged...
}

// waitForIdle polls the local server's session count until it
// reaches zero or maxWait elapses. Unexported — only Finish calls it.
func (d *Drainer) waitForIdle(maxWait time.Duration) {
    hostname, _ := os.Hostname()
    deadline := time.Now().Add(maxWait)
    ticker := time.NewTicker(5 * time.Second)
    defer ticker.Stop()

    for {
        own := d.Srv.Workers.WorkersForServer(hostname)
        sessions := d.Srv.Sessions.CountForWorkers(own)
        if sessions == 0 {
            slog.Info("finish: session count reached zero")
            return
        }
        if time.Now().After(deadline) {
            slog.Warn("finish: idle wait elapsed, proceeding with teardown",
                "remaining_sessions", sessions)
            return
        }
        <-ticker.C
    }
}
```

Main.go sets `FinishIdleWait` at drainer construction based on the
resolved backend:

```go
drainer = &drain.Drainer{
    // ...existing fields...
    FinishIdleWait: finishIdleWaitForBackend(srv.Backend, cfg),
}
```

where `finishIdleWaitForBackend` lives in `cmd/blockyard/` (same
package that already knows about concrete backend types via the
factory map) and returns `cfg.Update.DrainIdleWait.Duration` for
the process backend, or 5 minutes when `cfg.Update` is nil
(operator didn't declare `[update]` in TOML). For the Docker
backend it returns zero. Both the post-watchdog `doneCh` path
and the SIGUSR1 path call the same `drainer.Finish` — no new
entry points, no variant-aware call sites.

Why this shape, not a separate `FinishWhenIdle` public method: the
variant choice belongs in main.go, which already knows about both
backend types via the factory dispatch. Pushing the choice down into
call sites (`if processBackend { drainer.FinishWhenIdle } else {
drainer.Finish }`) would scatter it across the manual update
handler, the scheduled-update path, and the SIGUSR1 handler. Pushing
it up into the orchestrator factory would create a
`orchestrator → drain` cross-package dependency that doesn't otherwise
exist. Setting a field at construction time puts the choice at the
one site that already has the context to make it.

The SIGUSR1 path picks up the idle-wait automatically — a manual
drain on a process-backend server waits for sessions before tearing
down, same as the post-update drain. That's the correct behavior
(the old semantics would sever hijacked WebSockets immediately),
and it's a free consequence of the single-entry-point design.

`WorkersForServer(serverID)` is a new method on the `WorkerMap`
interface that returns only the worker IDs owned by the given server.
With Redis-backed worker state, `All()` returns workers from both
servers during the overlap; `WorkersForServer` filters to the caller's
own. The hostname is read on-demand rather than passed at construction
time — `os.Hostname()` is stable for the lifetime of the process, and
phase 3-3's `RedisWorkerMap` already uses the hostname as its server
identifier (`main.go:260` calls `server.NewRedisWorkerMap(rc, hostname)`),
so the two sites agree by construction.

Why add a method rather than extend `ActiveWorker` with a `ServerID`
field: the field approach would touch every caller that constructs
`ActiveWorker{...}` literals (mostly test fixtures in
`internal/ui/ui_test.go`), would force `MemoryWorkerMap` to carry a
field it never needs (single-node deployments have one server),
and would require `parseWorkerHash` to learn to read the already-
written `server_id` hash key. The interface method encapsulates all
of that behind one new function per implementation: Redis does a
SCAN + HGET for `server_id` (pattern lifted from the existing
`ForApp`), memory returns `All()` (single-node = all workers are
ours), no changes to `ActiveWorker` or any test fixture.

When sessions remain at `FinishIdleWait` timeout, the remaining
hijacked WebSocket connections are severed at the normal `Finish`
teardown — same as today's SIGTERM behavior, just delayed by up to
`FinishIdleWait`.

The 5-second poll interval means the Redis SCAN runs at most 12
times per minute during a drain, which is negligible compared to
the sustained traffic a running server already pushes through
Redis.

#### PID 1 detection (containerized mode skip)

Containerized blockyard runs as PID 1 in its container. Killing PID 1
stops the container regardless of what child processes do, so
fork+exec-ing a new blockyard inside the container is pointless —
the operator's container runtime (`docker compose up -d`, k8s
Deployment update) is the right tool for containerized rolling
updates.

The PID 1 check lives inside the candidate function appended by
`init()`, not in `init()` itself — the dispatcher (`newServerFactory`)
calls the candidate at orchestrator-construction time in `main()`, and
the candidate returns `nil` when `os.Getpid() == 1`. With both
candidates returning nil, the dispatcher returns `nil` as the
`ServerFactory`, and the admin endpoints return 501 with a clear
"containerized mode detected; use your container runtime's update
mechanism" message. (`init()` is too early to make policy decisions
that depend on the resolved backend type — the type assertion needs
the live `backend.Backend` value that only exists after the factory
map has been called.)

The Docker variant is unaffected — it requires `srv.Backend` to be
a `*docker.DockerBackend` and `ServerID() != ""`, which is only
true when blockyard has access to the Docker socket anyway.

#### Rollback: 501 in process variant

Rollback requires the previous version's binary. The Docker variant
pulls it from the registry; the process variant has no equivalent
(the previous binary is typically overwritten by the upgrade).
Phase 3-8 returns 501 from `/admin/rollback` when the active factory
is process, with a clear pointer to the manual procedure: restore
database backup, swap binaries, restart.

Adding a "previous binary path" config field was considered and
rejected — it couples blockyard to the operator's install scheme in
a way that no off-the-shelf install scheme provides.

### Step 7: Redis-backed resource allocators (ports + UIDs)

Phase 3-7's port and UID allocators (`internal/backend/process/ports.go`,
`internal/backend/process/uids.go`) are in-memory bitsets. Each server
keeps its own copy, scans from index 0, and hands out resources
independently. During a rolling-update overlap, both old and new
servers allocate concurrently:

- **UIDs**: guaranteed collision. No kernel-level probe exists for
  "is this UID in use", so the new server's fresh bitset has no way
  to detect that UID 60000 is already taken by an old-server worker.
  Two workers end up with the same host UID, silently weakening the
  per-worker isolation phase 3-7 advertises — the GID-based egress
  firewall still works (same GID), but "each worker has its own host
  UID" doesn't hold during cutover.
- **Ports**: probabilistic collision. Issue #173 tightens the
  pre-fork window via a held listener, but the "post-fork window"
  survives: both probes succeed for port 10500 (nothing actually
  holds it at probe time), both close their listeners, both
  `cmd.Start()` → one child wins at `bind()`, the other gets
  `EADDRINUSE` and the worker crashes. R startup is seconds-long,
  so that window is real during cutover.

The fix for both is the same pattern: **coordinate allocations via
Redis when Redis is configured; use the in-memory bitset otherwise**.
Rolling updates require Redis (phase 3-4's passive mode needs shared
session and worker state), so the no-Redis case has no cutover
overlap and therefore no cross-server collision. The in-memory
allocator stays correct for single-node deployments without change;
phase 3-8 adds a Redis-backed variant for each resource and selects
at construction time based on `cfg.Redis != nil`.

The port and UID allocators diverge in one detail: ports need a
kernel probe after the Redis claim (a non-blockyard host process
might have bound the port even though no blockyard peer has claimed
it), while UIDs don't (no analogous syscall). The port allocator
therefore has a retry loop around its Lua script; the UID allocator
is straight-line.

#### Interfaces

```go
type uidAllocator interface {
    Alloc() (int, error)
    Release(uid int) error
}

type portAllocator interface {
    // Reserve picks a free port and returns it plus a listener holding
    // the port. The caller MUST close the listener immediately before
    // cmd.Start (the #173 pattern) and call Release on the port when
    // the worker exits.
    Reserve() (port int, ln net.Listener, err error)
    Release(port int) error
}
```

The Reserve/Release naming for ports differs from Alloc/Release for
UIDs because port allocation has a two-phase handoff (claim → close
listener) that UIDs don't need.

#### Constructor wiring

`process.New` picks both implementations based on `cfg.Redis`:

```go
func New(ctx context.Context, fullCfg *config.Config) (*ProcessBackend, error) {
    // ...existing setup...
    cfg := fullCfg.Process
    if fullCfg.Redis != nil {
        client, err := redisstate.New(ctx, fullCfg.Redis)
        if err != nil {
            return nil, fmt.Errorf("redis: %w", err)
        }
        hostname, _ := os.Hostname()
        b.ports = newRedisPortAllocator(client,
            cfg.PortRangeStart, cfg.PortRangeEnd, hostname)
        b.uids = newRedisUIDAllocator(client,
            cfg.WorkerUIDStart, cfg.WorkerUIDEnd, hostname)
    } else {
        b.ports = newMemoryPortAllocator(
            cfg.PortRangeStart, cfg.PortRangeEnd)
        b.uids = newMemoryUIDAllocator(
            cfg.WorkerUIDStart, cfg.WorkerUIDEnd)
    }
    // ...
}
```

In-memory constructors wrap the existing bitset types with no
behavior changes beyond the Reserve signature update from #173.
Redis constructors capture the hostname as the "owner" identifier
so startup cleanup can distinguish own stale keys from peers'.

**Redis client ownership**: `process.New` opens its own
`redisstate.Client` rather than borrowing `srv.RedisClient` from
the server struct. The server's Redis client is created after
backend construction in today's main.go (`main.go:118` constructs
backend; `main.go:248` initializes Redis), and phase 3-8 does not
reorder that sequence — the reorder is a larger-surface change
than the wasted connection pool (~10 idle conns at go-redis
defaults) for one extra client, and the ordering dependency
elsewhere in main.go (Docker backend's preflight reads
`fullCfg.Redis.URL` as a string at construction time) makes the
reorder non-trivial to prove safe. `process.New` gains a
`context.Context` parameter so the Redis ping has a timeout; the
backend holds the client for the lifetime of the process and
relies on OS-level FD reclamation at exit — no new `Close()`
method on the Backend interface, because Redis writes through
`go-redis` are synchronous and there is nothing to flush.

#### Redis key schema

Two key namespaces, same shape:

```
{prefix}uid:<N>   ->  "<hostname>"    — claimed UID
{prefix}port:<N>  ->  "<hostname>"    — claimed port
```

No TTL on either. A key's presence means "claimed by the server
identified in the value." Release deletes the key. Crash recovery
lives in `CleanupOrphanResources` (below).

#### UID Alloc via Lua

Single atomic Lua script, same pattern as
`workermap_redis.go:countForAppScript`:

```lua
-- KEYS[1] = prefix, ARGV[1] = start, ARGV[2] = end, ARGV[3] = hostname
local prefix = KEYS[1]
local first = tonumber(ARGV[1])
local last = tonumber(ARGV[2])
local hostname = ARGV[3]
for i = first, last do
    local key = prefix .. "uid:" .. i
    if redis.call("SETNX", key, hostname) == 1 then
        return i
    end
end
return -1
```

One round-trip. Worst-case scans the full range (~1000 iterations),
which is fine inside a script — Redis Lua is fast, Alloc runs at
spawn time, and spawn is already bounded by R startup. Returns -1
when exhausted; Go translates to an error.

#### Port Reserve: Lua + kernel probe + retry

Ports need the probe because Redis only coordinates among blockyard
peers — a non-blockyard host process (legitimate or otherwise) can
still hold a port in our range. Flow:

1. Lua script: SETNX scan starting from `skip_from` (default 0),
   return first claimed port or -1.
2. Go caller attempts `net.Listen(":<port>")`.
3. If Listen succeeds, return (port, listener) — caller holds it
   per #173 and closes before `cmd.Start()`.
4. If Listen fails: the kernel says this port is externally busy.
   DEL the Redis key (don't leak the claim) and loop back to step 1
   with `skip_from = port + 1`, so the next Lua call skips past
   the index that just failed. Otherwise the script would hand out
   the same port repeatedly.
5. Loop until exhausted; then return an error.

Lua script with `skip_from`:

```lua
-- KEYS[1] = prefix, ARGV[1] = start, ARGV[2] = end,
-- ARGV[3] = hostname, ARGV[4] = skip_from
local prefix = KEYS[1]
local first = tonumber(ARGV[1])
local last = tonumber(ARGV[2])
local hostname = ARGV[3]
local skip_from = tonumber(ARGV[4])
local from = first
if skip_from > from then
    from = skip_from
end
for i = from, last do
    local key = prefix .. "port:" .. i
    if redis.call("SETNX", key, hostname) == 1 then
        return i
    end
end
return -1
```

Go caller:

```go
func (p *redisPortAllocator) Reserve() (int, net.Listener, error) {
    skipFrom := 0
    for {
        port, err := p.luaAlloc(skipFrom)
        if err != nil {
            return 0, nil, fmt.Errorf("redis alloc: %w", err)
        }
        if port < 0 {
            return 0, nil, errors.New("process backend: no free ports in range")
        }
        ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
        if err == nil {
            return port, ln, nil
        }
        // Kernel says this port is externally busy. Drop our claim
        // so future allocs (after the external holder releases) can
        // still use it, and skip past this index for this Spawn.
        _ = p.luaRelease(port)
        skipFrom = port + 1
    }
}
```

The probe failure is a rare safety-net path — the operator reserves
the port range for blockyard, so non-blockyard binds should be
exceptional. The retry loop bounds the recovery to O(range) Lua
calls in the worst case.

#### Release (both resources)

Release uses an ownership-checked Lua DEL so a hostname mismatch
doesn't accidentally delete a peer's key:

```lua
local key = KEYS[1]
local owner = ARGV[1]
if redis.call("GET", key) == owner then
    return redis.call("DEL", key)
end
return 0
```

One script, reused by both allocators with different key prefixes.
The ownership check is defensive — in the common case Release only
runs on keys the server itself allocated — but prevents corruption
under hostname drift.

#### Startup cleanup

Phase 3-7 added `CleanupOrphanResources(ctx) error` to the Backend
interface. The process backend's implementation is currently a
no-op. Phase 3-8 populates it for both allocators:

```go
func (b *ProcessBackend) CleanupOrphanResources(ctx context.Context) error {
    if alloc, ok := b.uids.(*redisUIDAllocator); ok {
        if err := alloc.CleanupOwnedOrphans(ctx); err != nil {
            return fmt.Errorf("uid cleanup: %w", err)
        }
    }
    if alloc, ok := b.ports.(*redisPortAllocator); ok {
        if err := alloc.CleanupOwnedOrphans(ctx); err != nil {
            return fmt.Errorf("port cleanup: %w", err)
        }
    }
    return nil
}
```

`CleanupOwnedOrphans` (on each allocator) scans its key namespace,
deletes entries whose value matches the local hostname AND whose
index is not in the local `b.workers` map. Handles the "previous
run crashed, Redis still has stale claims" case without touching
peer entries. Runs once at startup as part of `ops.StartupCleanup`.

Parallels phase 3-3's worker-map startup cleanup — same pattern,
applied per resource.

#### Caveats documented for operators

- The Redis allocators coordinate among blockyard peers, not among
  all host processes. A sysadmin manually using UIDs or ports inside
  the configured ranges would not be detected via Redis (though the
  port allocator's kernel probe catches the port case). Mitigation:
  reserve both ranges for blockyard in operator policy.
- Network partition between blockyard and Redis during Alloc/Reserve
  causes Spawn to fail with a clear error. The worker does not
  start; the caller retries or reports the failure. There is no
  silent fallback to the in-memory allocator — fallback would
  re-introduce the exact collisions the Redis allocators exist to
  prevent.
- The port allocator's retry loop on probe failure is bounded by
  the range size. In a pathological case (entire range externally
  busy) Reserve fails after scanning the whole range. Not a new
  failure mode — phase 3-7's in-memory allocator has the same
  worst case.

### Step 8: Deployment guides

Phase 3-8 delivers two operator-facing guides under
`docs/src/content/docs/guides/` and a short rolling-update addendum
in `docs/design/backends.md`. Contents are mostly mechanical and
don't need to be spec'd in detail here — the design concerns that
drive them are already covered by earlier steps.

#### `process-backend.md` (native mode)

Covers:

- Distro prerequisites (Debian/Ubuntu/Fedora/RHEL/Arch install
  commands for `bubblewrap`, `r-base`/`R`, `ca-certificates`,
  `iptables`; Alpine is not supported).
- Kernel/userns prerequisites (`kernel.unprivileged_userns_clone`).
- Minimal `blockyard.toml` example with `[server] backend = "process"`,
  `[process]` section populated with defaults, `[update] alt_bind_range`
  set.
- The iptables egress firewall from phase 3-7 — rationale,
  destination-scoped `--gid-owner` rules for Redis/OpenBao/DB/cloud
  metadata, the "do not use a blanket REJECT" warning, and the
  `iptables-restore` workflow.
- The `blockyard` system user and permissions on the data
  directory.
- The bwrap setuid requirement on Debian 12+/Ubuntu 24.04+
  (`chmod u+s /usr/bin/bwrap`) when not running as root.
  Cross-reference to phase 3-7's `checkBwrapHostUIDMapping`.
- A systemd unit template with `User=blockyard`, `Group=blockyard`,
  `Restart=on-failure`, and notes about `MemoryMax`/`CPUQuota` as
  shared ceilings (no per-worker cgroups in the process backend).
- Reverse proxy setup for rolling updates: static multi-upstream
  config fronting the primary bind + alt bind range, Caddy and
  Traefik examples, the pattern "list all ports in the upstream
  pool, let health checks pick the live one."
- Rolling update walkthrough (`by admin update`), prerequisites
  (Redis, proxy), failure modes.
- Limitations checklist: no per-worker resource limits, no
  per-worker network isolation, no automated rollback, no macOS
  support (use containerized mode).

#### `process-backend-container.md`

Covers:

- Image reference (`ghcr.io/cynkra/blockyard-process:<version>`).
- Why the outer-container seccomp profile is needed (bwrap's
  `--unshare-user` vs Docker's default) and how to extract the
  profile (`by admin install-seccomp` or `docker run --rm
  --entrypoint cat IMAGE /etc/blockyard/seccomp.json`; the
  `--entrypoint` override is required because the image's default
  entrypoint is `blockyard --config ...`).
- Full `docker-compose.yml` example with blockyard-process, Redis
  on an internal network, and Caddy fronting the primary bind port.
- Why the iptables egress firewall works differently in containerized
  mode (outer container's UID space vs host; cross-reference to
  `checkBwrapHostUIDMapping`) and the recommended mode (blockyard as
  PID 1 root in the container).
- A note that `by admin update` returns 501 in containerized mode
  and a pointer to `docker compose pull && docker compose up -d`
  or the relevant runtime's update mechanism.

#### `docs/design/backends.md` addendum

A short section in the existing process-backend block describing
the rolling-update split: native uses the orchestrator, containerized
uses the runtime. Cross-link the two guides above.

### Step 9: Tests

Five categories of new tests.

**Build-tag wiring.** `cmd/blockyard/build_tags_*_test.go` — one file
per variant with the matching `//go:build` tag set, asserting
`backendFactories` contains the expected entries and no others. Runs
as part of each variant's CI build.

**Dependency graph.** `internal/build/deps_test.go` — runs `go list
-deps -tags 'minimal,process_backend' ./cmd/blockyard` and asserts
the output excludes `github.com/moby/moby` and `internal/backend/docker`;
symmetric assertion for the docker variant. A third test confirms
the default build includes both. Catches regressions where an
untagged import accidentally pulls a backend into the wrong variant.

**Seccomp.** `cmd/seccomp-compile/main_test.go` — feeds a synthetic
OCI profile, compiles to BPF, and round-trips the result back via
libseccomp's disassembler. Verifies actions, syscall names, and
argument matchers survive. `docker/seccomp_test.go` — parses
`docker/blockyard-seccomp.json` (outer profile) and asserts the
unconditional allow rule for `clone`/`unshare` exists. Catches
accidental edits that re-introduce the cap gating.
`internal/backend/process/seccomp_integration_test.go`
(`//go:build process_test`) — applies the compiled BPF to a real
bwrap-spawned worker and verifies a blocked syscall (e.g., `mount`)
returns `EPERM`. Skipped when bwrap is unavailable.

**Resource allocators.** Four new unit-test files under
`internal/backend/process/`, all against `miniredis`:

- `uids_redis_test.go` — Alloc/Release, exhaustion errors, and
  concurrent-alloc from N goroutines returning distinct UIDs.
- `uids_cleanup_test.go` — populate Redis with keys owned by the
  local hostname, a peer hostname, and a mix of live+stale local
  UIDs, then call `CleanupOwnedOrphans` and assert only stale
  local entries are removed.
- `ports_redis_test.go` — Reserve/Release, exhaustion, concurrent
  Reserve returning distinct ports, **probe-failure retry loop**
  (test pre-binds a port in the configured range on the host, then
  calls Reserve and verifies the allocator skips past it via the
  `skip_from` mechanism and returns a different port).
- `ports_cleanup_test.go` — mirror of `uids_cleanup_test.go` for
  the port key namespace.

The concurrent-alloc tests are the critical correctness check —
they verify the Lua script's atomicity under contention, which is
the whole reason we moved to Redis in the first place.

**Process orchestrator.**
`internal/orchestrator/clone_process_test.go` — unit tests for
`pickAltBind`, env helpers, `processInstance.Addr` loopback rewrite,
and `Kill` timeout escalation.

`internal/orchestrator/process_integration_test.go`
(`//go:build process_test`) — end-to-end rolling update test.
Before the test body runs, `TestMain` calls `go build -o
<tempdir>/blockyard ./cmd/blockyard` once to produce a real
blockyard binary (caching defeats the build cost on repeat runs),
then overrides a test seam on `processServerFactory`:

```go
// clone_process.go (production)
var executableFn = os.Executable // overridable in tests

func (f *processServerFactory) CreateInstance(...) {
    self, err := executableFn()
    // ...
}
```

The test writes:

```go
func TestMain(m *testing.M) {
    bin := buildBlockyardBinary() // go build to t.TempDir()
    orchestrator.ExecutableFnForTest = func() (string, error) { return bin, nil }
    os.Exit(m.Run())
}
```

(`ExecutableFnForTest` is an exported test seam on the
orchestrator package that assigns to the unexported
`executableFn`.) This means `os.Executable()` inside the running
test binary never gets called from the factory — it always
returns the pre-built blockyard path — and the child process is
a real blockyard reading the test's miniredis instance.

Flow:
1. Start an in-process Redis via `miniredis` (`github.com/alicebob/miniredis/v2`).
   `testcontainers` is not viable here — phase 3-7's CI runs the
   process backend tests inside the `ghcr.io/rocker-org/r-ver:4.4.3`
   container, which has no Docker socket to spawn child containers.
2. Start an old blockyard with `backend = "process"` against the
   miniredis instance.
3. POST `/api/v1/admin/update` with a mocked GitHub check returning
   "update available".
4. Verify the orchestrator fork+execs a new blockyard on an alt
   bind, polls `/readyz`, calls `/admin/activate`, and enters
   watchdog mode.
5. Verify the old server's `/healthz` flips to 503 and the new
   server's `/healthz` stays 200.
6. Drive a fake session that ends; verify `Finish` (with
   `FinishIdleWait` set) detects zero sessions and proceeds with
   teardown.
7. Verify the new server is still running after the old exits.
8. While both servers are alive, spawn workers on each and verify
   both the UIDs AND the ports handed out are disjoint — exercises
   both Redis-backed allocators' cross-server coordination end-to-
   end, not just the unit tests.

**CI smoke** — `server-smoke` job in `release.yml` pulls each
variant image and hits `/healthz` (see step 5).

## Files changed

| File | Action | Summary |
|------|--------|---------|
| `cmd/blockyard/main.go` | update | Replace direct backend imports + switch with factory map lookup; store `cfg.ConfigPath`; PID 1 detection disables the orchestrator factory. |
| `cmd/blockyard/backend_docker.go` | create | `!minimal \|\| docker_backend`. init() registers docker factory. |
| `cmd/blockyard/backend_process.go` | create | `!minimal \|\| process_backend`. init() registers process factory. |
| `cmd/blockyard/orchestrator.go` | create | Untagged. `orchestratorFactoryFns` slice and `newServerFactory` dispatcher. |
| `cmd/blockyard/orchestrator_docker.go` | create | `!minimal \|\| docker_backend`. init() appends Docker orchestrator candidate. |
| `cmd/blockyard/orchestrator_process.go` | create | `!minimal \|\| process_backend`. init() appends process orchestrator candidate. |
| `cmd/blockyard/build_tags_*_test.go` | create | Three files, one per variant, verify registered factories match. |
| `cmd/seccomp-compile/main.go` | create | ~120-line Go program using libseccomp-golang. Reads OCI seccomp JSON, emits BPF. CGO at build time only. |
| `cmd/seccomp-compile/main_test.go` | create | Round-trip test. |
| `cmd/seccomp-merge/main.go` | create | ~80-line Go program (no CGO) that merges upstream moby profile + blockyard overlay. |
| `internal/build/deps_test.go` | create | Asserts `go list -deps` output excludes the right packages per variant. |
| `internal/orchestrator/orchestrator.go` | update | Drop the `docker dockerClient` and `serverID string` fields and the `dockerClient` interface itself; take a `ServerFactory` instead. Every method that touches `o.docker` (`pullImage`, `currentImageBase`, `currentImageTag`, `containerAddr`, `killAndRemove`, `cloneConfig`, `startClone`) either moves into `clone_docker.go` or routes through the factory. `Update`/`Watchdog`/`Rollback` use the factory. Add `activeInstance newServerInstance` field — set by `Update` from `CreateInstance`'s return, read by `Watchdog` for polling/kill and cleared on return; `Rollback` uses it within one call. Public API collapses: `Update` returns `(bool, error)` instead of `(*UpdateResult, error)`; `Watchdog` drops `newID`/`newAddr` params and reads from `activeInstance`. `UpdateResult` type is removed. The `New` constructor's signature changes (drops the `*client.Client` and `serverID` args, adds a `ServerFactory`); all call sites in `cmd/blockyard/main.go` and `orchestrator_test.go` move with it. This is the largest non-test code chunk in the phase. |
| `internal/orchestrator/serverfactory.go` | create | `ServerFactory` and `newServerInstance` interfaces. |
| `internal/orchestrator/clone.go` | delete | Replaced by `clone_docker.go`. |
| `internal/orchestrator/clone_docker.go` | create | `!minimal \|\| docker_backend`. Docker factory, instance, container clone, image pull, kill. |
| `internal/orchestrator/clone_process.go` | create | `!minimal \|\| process_backend`. Process factory, fork+exec, `pickAltBind`, env helpers. Package-level `var executableFn = os.Executable` as a test seam so `process_integration_test.go` can point the factory at a pre-built blockyard binary. |
| `internal/orchestrator/clone_process_test.go` | create | Unit tests for process factory internals. |
| `internal/orchestrator/export_test.go` | create | Exports `ExecutableFnForTest` (assigns to unexported `executableFn`) so the integration test can inject a pre-built blockyard binary path from its `TestMain`. |
| `internal/orchestrator/process_integration_test.go` | create | `process_test`. End-to-end rolling update against real Redis. |
| `internal/orchestrator/helpers.go` | update | Keep `waitReady`/`activate`/`checkReady`/`generateActivationToken`. `waitReady` signature changes from `(ctx, containerID) (addr, err)` to `(ctx, addr) error` — the caller passes an already-resolved address (cached on `newServerInstance.Addr()` at `CreateInstance` time), and `waitReady` only polls `/readyz`. Move Docker-specific helpers (`pullImage`, `containerAddr`, `killAndRemove`, `currentImageBase/Tag`) to `clone_docker.go`; the inspect-retry loop that used to live in `waitReady` moves into `dockerServerFactory.CreateInstance`. |
| `internal/orchestrator/rollback.go` | update | Factory-driven restart; 501 path for the process factory. |
| `internal/orchestrator/orchestrator_test.go` | update | Mock `ServerFactory` instead of `dockerClient`. |
| `internal/drain/drain.go` | update | Add `FinishIdleWait time.Duration` field on the `Drainer` struct; `Finish` calls an unexported `waitForIdle(maxWait)` helper when the field is non-zero. `waitForIdle` polls `Srv.Workers.WorkersForServer(hostname)` + `Srv.Sessions.CountForWorkers(own)` at 5s intervals until zero sessions or the timeout elapses. No new public method, no new call sites. |
| `internal/drain/drain_test.go` | update | Tests for the idle-wait prelude: `Finish` with `FinishIdleWait = 0` matches today's behavior; with non-zero, it waits for zero sessions before tearing down; timeout path proceeds with remaining sessions logged. |
| `cmd/blockyard/main.go` | update | (additional to existing factory-map changes) Set `drainer.FinishIdleWait` at construction based on the resolved backend type — `cfg.Update.DrainIdleWait.Duration` for process (falling back to 5 min when `cfg.Update` is nil), zero for docker. The helper `finishIdleWaitForBackend` lives in a small file in `cmd/blockyard/` that type-asserts against the concrete backend. |
| `internal/config/config.go` | update | Add `UpdateConfig.AltBindRange` (default `"8090-8099"`), `UpdateConfig.DrainIdleWait` (default `5m` via `updateDefaults`), `Config.ConfigPath` (programmatic, no TOML tag). |
| `internal/units/portrange.go` | create | Shared port range parser. |
| `internal/api/admin.go` | update | `handleAdminUpdate` adapts to the collapsed `Update (bool, error)` signature and drops the `UpdateResult`/`ContainerID` plumbing — the admin goroutine no longer threads an opaque instance through the Update → Watchdog hand-off. `handleAdminRollback` returns 501 for the process factory variant. |
| `internal/server/workermap_iface.go` | update | Add `WorkersForServer(serverID string) []string` to the `WorkerMap` interface. |
| `internal/server/workermap_redis.go` | update | Implement `WorkersForServer` via SCAN + HGET on the `server_id` hash field (pattern lifted from the existing `ForApp`). The field is already written by `Set` — phase 3-3 populates it but nothing reads it back. This is the reader. |
| `internal/server/workermap_memory.go` | update | Implement `WorkersForServer` as `m.All()` — in single-node mode every worker belongs to "this" server, so the filter is a no-op. |
| `internal/backend/process/uids.go` | update | Refactor `uidAllocator` into an interface; rename the existing bitset type to `memoryUIDAllocator` with no behavior changes. Add `newMemoryUIDAllocator` constructor. |
| `internal/backend/process/uids_redis.go` | create | `redisUIDAllocator` — Redis-backed implementation. Lua script for atomic SETNX scan, shared ownership-checked DEL script, `CleanupOwnedOrphans` for startup cleanup. |
| `internal/backend/process/uids_redis_test.go` | create | Unit tests against miniredis: alloc/release, exhaustion, concurrent alloc returns distinct UIDs. |
| `internal/backend/process/uids_cleanup_test.go` | create | `CleanupOwnedOrphans` scoping — removes own stale entries, leaves peer entries alone. |
| `internal/backend/process/ports.go` | update | Refactor `portAllocator` into an interface with `Reserve() (port, ln, err)` + `Release`; rename the existing bitset type to `memoryPortAllocator`. The Reserve signature matches the #173 held-listener pattern. |
| `internal/backend/process/ports_redis.go` | create | `redisPortAllocator` — Redis-backed implementation. SETNX Lua script with `skip_from` argument for probe-retry, kernel-probe retry loop in Go, shared ownership-checked DEL script, `CleanupOwnedOrphans` for startup cleanup. |
| `internal/backend/process/ports_redis_test.go` | create | Unit tests against miniredis: Reserve/Release, exhaustion, concurrent Reserve returns distinct ports, probe-failure retry loop exercise (synthetic collision via pre-bound listener). |
| `internal/backend/process/ports_cleanup_test.go` | create | `CleanupOwnedOrphans` scoping — mirror of the UID cleanup test. |
| `internal/backend/process/process.go` | update | `New(ctx, cfg)` picks both allocator implementations based on `fullCfg.Redis`; when Redis is configured `New` opens its own `redisstate.Client` (ordering in main.go keeps backend construction ahead of `redisstate.New`, so borrowing `srv.RedisClient` is not available at construction time — see step 7 discussion). `CleanupOrphanResources` delegates to each Redis allocator when it's the active one. Spawn uses `Reserve()` and closes the held listener immediately before the fork goroutine's `cmd.Start()`. |
| `docker/server.Dockerfile` | update | Add `BUILD_TAGS` build arg defaulting to docker variant tags. |
| `docker/server-process.Dockerfile` | create | Process-backend image. rocker/r-ver base, bwrap, BPF profile, `-tags 'minimal,process_backend'`. |
| `docker/server-everything.Dockerfile` | create | Both backends. rocker/r-ver base, R + bwrap + iptables, default tags. |
| `docker/blockyard-seccomp.json` | create | Outer-container seccomp profile. Generated from upstream + overlay. Committed. |
| `docker/blockyard-seccomp-overlay.json` | create | Hand-edited overlay (~20 lines). |
| `docker/blockyard-bwrap-seccomp.json` | create | Bwrap seccomp profile (JSON source for BPF compile). |
| `docker/blockyard-bwrap-seccomp-overlay.json` | create | Overlay for bwrap profile. |
| `docker/upstream-default-seccomp.json` | create | Vendored moby `default.json`. Regenerated by `make regen-seccomp`. |
| `docker/seccomp_test.go` | create | Parses the outer profile and asserts the expected relaxations exist. |
| `Makefile` | create or update | `regen-seccomp` target. |
| `.github/workflows/server.yml` | update | 6-entry matrix (3 variants × 2 architectures). |
| `.github/workflows/release.yml` | update | Per-variant Trivy scans and manifest publishing. Add `seccomp-blob` job that uploads the BPF as a release asset. Add `server-smoke` job. |
| `.github/workflows/ci.yml` | update | Variant build-tag tests, dep-graph tests, `make regen-seccomp` drift check. |
| `cmd/by/admin.go` | update | `by admin install-seccomp [--target]` subcommand. |
| `internal/by/seccomp_embed.go` | create | `//go:embed`s the outer profile into the `by` binary. |
| `docs/src/content/docs/guides/process-backend.md` | create | Native deployment guide. |
| `docs/src/content/docs/guides/process-backend-container.md` | create | Containerized deployment guide. |
| `docs/design/backends.md` | update | Rolling-update section cross-linking the new guides. |
| `docs/design/v3/plan.md` | (done) | Deliverables #4 and #5 already rewritten. |

## Design decisions

1. **Three image variants, not two.** A pure two-variant scheme
   (`blockyard-docker` slim + `blockyard-process` with R) forces
   operators who want both backends available to pick one and accept
   the other is broken. The everything variant is the default
   `go build` output — already produced for development — and gives
   operators a "doesn't matter, I'll decide later" option that still
   works. The soft `:latest` migration is the cost; release notes
   and the sed command bound it.

2. **Positive build tags with a `minimal` mode switch.** Negative
   tags (`no_docker`, `no_process`) don't scale past two backends —
   adding k8s would produce an ugly `no_kubernetes`. Positive tags
   (`docker_backend`, etc.) read naturally and add by appending. The
   `minimal` mode is the trick that lets default `go build` still
   produce the everything binary. The expression
   `!minimal || docker_backend` reads as "include unless someone
   asked for a minimal build and didn't pick this."

3. **Build tags only at the seams.** Internal backend packages
   (`internal/backend/docker/`, `internal/backend/process/`) carry no
   build tags. They're normal Go packages that enter the dependency
   graph only when something with a passing tag imports them. Tags
   live in `cmd/blockyard/` (factory registration) and
   `internal/orchestrator/` (clone variant files). The backend code
   itself stays readable.

4. **Slice-of-init() factory pattern.** In the everything variant,
   both backend wrapper files compile into the same package and
   cannot share a top-level function name. Each file appends a
   candidate function to a package-level slice from `init()`; the
   dispatcher picks the first non-nil candidate. Slice order is
   irrelevant — each candidate returns nil unless its backend is
   active.

5. **Two seccomp profiles, one compile pipeline.** The outer-container
   profile (JSON, applied by Docker) and the bwrap-internal profile
   (BPF, applied by bwrap inside the sandbox) target different threat
   surfaces and diverge in their relaxations. Merging them would
   either weaken the inner sandbox (allowing user-namespace creation
   that workers have no business doing) or break the outer (refusing
   to allow it to bwrap itself). They share the structural source
   (Docker's default) via the same vendored-upstream + overlay
   pattern.

6. **CGO at build time, not runtime.** `cmd/seccomp-compile` requires
   `libseccomp-golang` and `libseccomp-dev`, which means CGO. It
   runs in a single Dockerfile build stage producing the BPF blob;
   the runtime blockyard binary stays CGO-disabled. Pure-Go OCI-to-BPF
   compilers exist but are less mature — reimplementing libseccomp's
   hardened C is a larger risk than the CGO dependency.

7. **Vendored upstream + overlay + merge tool.** Docker's default
   profile evolves with kernel features. Hand-maintaining our own
   from scratch would be a perpetual sync chore. Vendor + overlay
   makes the sync mechanical: bump moby in `go.mod`, run
   `make regen-seccomp`, overlay applies cleanly or CI fails noisily.

8. **Two-port parallel servers, not single-port handoff.** The
   alternative (single-port handoff via `SO_REUSEPORT` or
   close-then-bind) is simpler in the proxy config but non-trivial
   in kernel coordination. Two-port parallel servers + reverse proxy
   with health-based routing is what phase 3-7 step 8 described and
   what actually delivers zero-interruption updates. Operators who
   want rolling updates already run a proxy with health checks for
   the Docker variant, so the incremental cost is minimal.

9. **`update.alt_bind_range` separate from `[process] port_range`.**
   Both servers allocate workers from the same worker port pool
   during the overlap, so that pool is already under pressure.
   Borrowing one slot for the alt bind would compete with worker
   capacity at exactly the wrong moment. A separate 10-port range
   keeps concerns orthogonal.

10. **No `Pdeathsig` on the new server process.** The whole point
    is for the new server to outlive the old. `Pdeathsig = SIGKILL`
    would kill the new server when the old exits. The new server
    orphans cleanly to init/systemd via standard Linux reparenting.
    `Setsid: true` puts the new server in its own process group so
    signals to the old server's pgrp don't propagate.

11. **Process orchestrator is native-mode-only; containerized mode
    returns 501 with a clear pointer.** Containerized blockyard runs
    as PID 1; killing PID 1 stops the container regardless of child
    process tricks. The container runtime's update mechanism
    (`docker compose up -d`, k8s Deployment update, nomad job
    update) is the right tool there. Phase 3-8 detects PID 1 at
    startup and disables the process orchestrator factory.

12. **No automatic rollback in the process variant.** Rollback
    requires the previous version's binary, and the process variant
    has no equivalent of pulling an old Docker image. Adding a
    "previous binary path" config couples blockyard to the operator's
    install scheme in a way no off-the-shelf install scheme provides.
    Manual procedure (restore backup, swap binaries, restart) is
    documented in the native deployment guide.

13. **Idle-wait as a `Finish` prelude controlled by a struct field,
    not a separate `FinishWhenIdle` public method.** The process
    backend's cutover needs `Finish` to wait for sessions to end
    before tearing down; the Docker backend cuts over hard and
    relies on the reverse proxy. A separate public method would
    force every call site (manual update handler, scheduled-update
    path, SIGUSR1 handler) to know which variant it's in — scattering
    the variant-awareness across the codebase. A field on the
    `Drainer` struct, set once at construction time in main.go
    (which already dispatches backends via the factory map), keeps
    the variant choice in one place. `Finish` checks the field and
    delegates to an unexported `waitForIdle` helper when non-zero.
    SIGUSR1 picks up the behavior automatically, which is the right
    default for the process backend (a manual drain shouldn't sever
    hijacked WebSockets).

    Session count is the right unit of wait, not request count — a
    "wait until in-flight requests drain" mechanism wouldn't
    capture WebSocket sessions (long-lived, hold workers for their
    entire duration). Filtered to workers owned by the local
    hostname so the old server doesn't wait on the new server's
    workers. Ownership lookup goes through the new
    `WorkerMap.WorkersForServer` method rather than adding a
    `ServerID` field to `ActiveWorker` — the interface method keeps
    the in-memory map one-line simple (single-node = all workers
    are ours) and avoids touching every `ActiveWorker{...}`
    construction site in tests.

    Timeout source is a new `[update] drain_idle_wait` field
    (default 5 minutes), not a reuse of `ShutdownTimeout` or
    `DrainTimeout`. Neither existing field maps cleanly onto "wait
    for sessions to finish during a rolling update": `DrainTimeout`
    is already passed to `Finish` as its HTTP-teardown budget, and
    reusing it would couple "idle wait" and "teardown" into one
    knob with `2 * DrainTimeout` worst case; `ShutdownTimeout` is
    semantically the SIGTERM budget and reusing it would make
    SIGTERM shutdown and rolling-update idle-wait co-vary. The
    new field lives in `[update]` alongside `watch_period` and
    `alt_bind_range` so all process-orchestrator tuning knobs
    cluster together.

14. **Port and UID allocators: Redis when present, in-memory
    otherwise; no fallback.** Both resources can collide across
    servers during a rolling-update cutover. UIDs collide
    deterministically (no kernel probe exists), ports collide
    probabilistically (the probe succeeds on both sides during the
    seconds-long R startup window). Both are closed by the same
    pattern — Lua-scripted SETNX scan, one-key-per-resource with
    hostname ownership, shared ownership-checked DEL, per-allocator
    startup cleanup. The in-memory variants are retained for
    single-node deployments where the problem doesn't exist:
    rolling updates require Redis (phase 3-4 passive mode reads
    shared state from it), so no-Redis ⇒ no-cutover ⇒ no collision.
    A silent fallback to in-memory under network partition would
    re-introduce the exact bugs the Redis allocators exist to
    prevent, so Alloc/Reserve surface Redis errors as Spawn
    errors rather than falling back.

    The port allocator layers one extra mechanism on top of the
    shared pattern: a kernel-probe retry loop. Redis only
    coordinates among blockyard peers — a non-blockyard host
    process binding a port in the range is invisible to Redis but
    visible to `net.Listen`. The Reserve loop SETNX → `net.Listen`
    → on failure DEL and advance a `skip_from` index → retry.
    UIDs have no equivalent retry because no equivalent syscall
    exists. This is also why the #173 held-listener pattern still
    applies: Redis closes the cross-server window, #173 closes
    the cross-process-on-the-same-host window, and the two
    compose cleanly.

    Considered and rejected: unifying ports and UIDs under a
    single generic `redisResourceAllocator[T]`. The shared portion
    is ~30 lines (Lua SETNX, Lua DEL, cleanup scan); the port-
    specific retry loop and Reserve/Release vs Alloc/Release
    signature asymmetry would need either a callback indirection
    or ugly type parameters. Two files with visible duplication
    read better than one clever file with conditional behavior.

15. **rocker/r-ver as the base image, not Debian + manual R.** Rocker
    maintains R-on-Linux images with the right system libraries,
    `R_LIBS` paths, and locale setup for R numerics. Reproducing this
    by hand is fragile across R versions and package dependencies.
    The marginal size saving isn't worth the maintenance burden.
    Alpine + R is not viable — R on musl has numerics and locale
    issues.

16. **Three explicit Dockerfiles, not one with `ARG` switches.**
    Dockerfile conditionals via `ARG`-driven shell tricks make
    builds harder to read and harder to cache predictably with
    buildx. Three files have visible duplication in the early
    stages but auditable structure.
