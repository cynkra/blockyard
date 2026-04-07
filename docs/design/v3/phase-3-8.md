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
  Without Redis, rolling updates are not available — `by admin update`
  prints the manual restart instructions, same as the Docker variant.
- **Phase 3-4** — drain mode, passive mode (`BLOCKYARD_PASSIVE=1`),
  the three-method `Drain()` / `Finish()` / `Shutdown()` lifecycle,
  and `Undrain()`. Phase 3-8 adds one sibling (`FinishWhenIdle`) but
  leaves the existing methods untouched.
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

8. **Native and containerized deployment guides**
   (`docs/src/content/docs/guides/process-backend.md`,
   `.../process-backend-container.md`) — operator docs: distro
   prerequisites, egress firewall rules, system user creation, systemd
   unit, reverse proxy setup for rolling updates, limitations, and
   (containerized) the seccomp profile extraction workflow.
   `docs/design/backends.md` gains a short rolling-update section
   cross-linking the guides.

9. **Tests** — build-tag wiring, dependency-graph exclusion check,
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

type backendFactory func(ctx context.Context, cfg *config.Config) (backend.Backend, error)

func main() {
    // ...load config, etc...
    factory, ok := backendFactories[cfg.Server.Backend]
    if !ok {
        slog.Error("backend not available in this build",
            "backend", cfg.Server.Backend,
            "available", availableBackends())
        os.Exit(1)
    }
    be, err := factory(ctx, cfg)
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
    backendFactories["docker"] = func(ctx context.Context, cfg *config.Config) (backend.Backend, error) {
        return docker.New(ctx, &cfg.Docker, cfg.Storage.BundleServerPath)
    }
}
```

`backend_process.go` mirrors this for `process.New(cfg)`. When the
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
- `internal/orchestrator/serverfactory.go` (new, untagged) — defines
  the `ServerFactory` and `newServerInstance` interfaces the core
  uses to delegate "create a new server instance":

  ```go
  type ServerFactory interface {
      CreateInstance(ctx context.Context, ref string, extraEnv []string, sender task.Sender) (newServerInstance, error)
      PreUpdate(ctx context.Context, version string, sender task.Sender) error
  }

  type newServerInstance interface {
      ID() string            // stable identifier for logging
      Addr() string          // host:port the orchestrator will poll/activate
      Kill(ctx context.Context) // tear down on failure or watchdog rollback
  }
  ```

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
  process-backend image. Based on `rocker/r-ver:4.4.0` (see
  rationale below). Installs `bubblewrap`, `ca-certificates`,
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
  `rocker/r-ver:4.4.0` since R is the expensive dependency and
  including it makes the `iptables` tooling cheap by comparison.

Key `seccomp-compiler` stage (shared by process and everything
variants):

```dockerfile
FROM golang:1.25.8-alpine AS seccomp-compiler
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
          docker run --rm "$IMAGE" cat /etc/blockyard/seccomp.json > /tmp/seccomp.json
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
3. `Update` generates an activation token and calls
   `factory.CreateInstance(ctx, version, []string{...}, sender)`.
   For the process variant, this picks a free port from
   `[update] alt_bind_range`, resolves `os.Executable()`, and
   `cmd.Start()`s a new blockyard child with an env containing
   `BLOCKYARD_PASSIVE=1`, `BLOCKYARD_BIND=0.0.0.0:<altport>`, and
   `BLOCKYARD_ACTIVATION_TOKEN=<token>`. Everything else from the
   old server's env is copied. `Setsid: true`, no `Pdeathsig`.
4. `waitReady` polls `/readyz` on the new instance's addr (via
   loopback, `127.0.0.1:<altport>`) until 200.
5. `drainFn()` on the old server (health → 503). The operator's
   reverse proxy stops routing new traffic to the old port.
6. `activate(ctx, newAddr)` posts to `/admin/activate` on the new
   instance with the activation token.
7. The orchestrator enters watchdog mode. When the watch period
   elapses and the new instance is healthy, the orchestrator polls
   the old server's local session count via `FinishWhenIdle` and
   exits when the count reaches zero (or a timeout elapses).
8. The new server, being a child of the old server but *without*
   `Pdeathsig`, survives the old's exit. Its parent becomes
   init/systemd. The new server's autoscaler rebuilds the worker
   pool from new traffic.

#### Alt bind range config

New field in `UpdateConfig`:

```go
type UpdateConfig struct {
    Schedule     string   `toml:"schedule"`
    Channel      string   `toml:"channel"`
    WatchPeriod  Duration `toml:"watch_period"`
    AltBindRange string   `toml:"alt_bind_range"` // e.g. "8090-8099"
}
```

Default `"8090-8099"` in `applyDefaults()`. Parsing and free-port
selection go through a new shared helper `internal/units/portrange.go`
(used by both the worker port range and the alt bind range).

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
    env = setEnv(env, "BLOCKYARD_BIND", altBind)
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

#### `Drainer.FinishWhenIdle`

Phase 3-4's `Finish()` shuts down HTTP listeners immediately and
severs hijacked WebSocket connections. The process orchestrator
needs softer behavior: wait until sessions end naturally, then call
`Finish()`.

```go
func (d *Drainer) FinishWhenIdle(ctx context.Context, maxWait time.Duration) {
    deadline := time.Now().Add(maxWait)
    ticker := time.NewTicker(5 * time.Second)
    defer ticker.Stop()

    for {
        own := d.ownWorkers() // filters srv.Workers to local hostname
        sessions := d.Srv.Sessions.CountForWorkers(own)
        if sessions == 0 {
            slog.Info("finish when idle: session count reached zero")
            break
        }
        if time.Now().After(deadline) {
            slog.Warn("finish when idle: max wait elapsed",
                "remaining_sessions", sessions)
            break
        }
        select {
        case <-ctx.Done(): return
        case <-ticker.C:
        }
    }
    d.Finish(d.Srv.Config.Server.DrainTimeout.Duration)
}
```

`ownWorkers` filters `srv.Workers.All()` to entries whose
`ServerID` matches the local hostname. With Redis-backed worker
state, `All()` returns workers from both servers during the overlap;
the old server's drain logic only cares about its own.

`ServerID` on `ActiveWorker` comes from phase 3-3's Redis worker map
(the hostname of the owning server, populated at `Workers.Set` time).
Verify against the actual phase 3-3 implementation; if the field
doesn't exist yet, phase 3-8 adds it.

The max-wait reuses `cfg.Server.ShutdownTimeout`. When sessions
remain at timeout, the remaining hijacked WebSocket connections are
severed at `Finish()` time — same as today's `SIGTERM` behavior.

#### PID 1 detection (containerized mode skip)

Containerized blockyard runs as PID 1 in its container. Killing PID 1
stops the container regardless of what child processes do, so
fork+exec-ing a new blockyard inside the container is pointless —
the operator's container runtime (`docker compose up -d`, k8s
Deployment update) is the right tool for containerized rolling
updates.

The process orchestrator factory's `init()` check includes
`os.Getpid() == 1`: when true, the factory returns nil and the
admin endpoints return 501 with a clear "containerized mode detected;
use your container runtime's update mechanism" message.

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

### Step 7: Deployment guides

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
  profile (`by admin install-seccomp` or `docker run --rm IMAGE cat
  /etc/blockyard/seccomp.json`).
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

### Step 8: Tests

Four categories of new tests.

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

**Process orchestrator.**
`internal/orchestrator/clone_process_test.go` — unit tests for
`pickAltBind`, env helpers, `processInstance.Addr` loopback rewrite,
and `Kill` timeout escalation.
`internal/orchestrator/process_integration_test.go`
(`//go:build process_test`) — end-to-end rolling update test:
1. Start a real Redis (`miniredis` or testcontainers).
2. Start an old blockyard with `backend = "process"` against the
   real Redis.
3. POST `/api/v1/admin/update` with a mocked GitHub check returning
   "update available".
4. Verify the orchestrator fork+execs a new blockyard on an alt
   bind, polls `/readyz`, calls `/admin/activate`, and enters
   watchdog mode.
5. Verify the old server's `/healthz` flips to 503 and the new
   server's `/healthz` stays 200.
6. Drive a fake session that ends, verify `FinishWhenIdle` detects
   zero sessions and exits.
7. Verify the new server is still running after the old exits.

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
| `internal/orchestrator/orchestrator.go` | update | Drop `dockerClient`/`serverID`; take a `ServerFactory`. `Update`/`Watchdog`/`Rollback` use the factory. |
| `internal/orchestrator/serverfactory.go` | create | `ServerFactory` and `newServerInstance` interfaces. |
| `internal/orchestrator/clone.go` | delete | Replaced by `clone_docker.go`. |
| `internal/orchestrator/clone_docker.go` | create | `!minimal \|\| docker_backend`. Docker factory, instance, container clone, image pull, kill. |
| `internal/orchestrator/clone_process.go` | create | `!minimal \|\| process_backend`. Process factory, fork+exec, `pickAltBind`, env helpers. |
| `internal/orchestrator/clone_process_test.go` | create | Unit tests for process factory internals. |
| `internal/orchestrator/process_integration_test.go` | create | `process_test`. End-to-end rolling update against real Redis. |
| `internal/orchestrator/helpers.go` | update | Keep `waitReady`/`activate`/`checkReady`/`generateActivationToken`. Move Docker-specific helpers to `clone_docker.go`. |
| `internal/orchestrator/rollback.go` | update | Factory-driven restart; 501 path for the process factory. |
| `internal/orchestrator/orchestrator_test.go` | update | Mock `ServerFactory` instead of `dockerClient`. |
| `internal/drain/drain.go` | update | Add `FinishWhenIdle(ctx, maxWait)` and `ownWorkers()` helper. |
| `internal/drain/drain_test.go` | update | Tests for `FinishWhenIdle`. |
| `internal/config/config.go` | update | Add `UpdateConfig.AltBindRange` (default `"8090-8099"`), `Config.ConfigPath` (programmatic, no TOML tag). |
| `internal/units/portrange.go` | create | Shared port range parser. |
| `internal/api/admin.go` | update | `handleAdminRollback` returns 501 for the process factory variant. |
| `internal/server/server.go` | update | Ensure `ActiveWorker.ServerID` exists and is populated (verify against phase 3-3). |
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

13. **`FinishWhenIdle` waits on session count, not request count.**
    A "wait until in-flight requests drain" mechanism wouldn't
    capture WebSocket sessions (long-lived, hold workers for their
    entire duration). Session count is the right unit — it
    corresponds to "things a user is currently doing". Filtered to
    workers owned by the local hostname so the old server doesn't
    wait on the new server's workers.

14. **rocker/r-ver as the base image, not Debian + manual R.** Rocker
    maintains R-on-Linux images with the right system libraries,
    `R_LIBS` paths, and locale setup for R numerics. Reproducing this
    by hand is fragile across R versions and package dependencies.
    The marginal size saving isn't worth the maintenance burden.
    Alpine + R is not viable — R on musl has numerics and locale
    issues.

15. **Three explicit Dockerfiles, not one with `ARG` switches.**
    Dockerfile conditionals via `ARG`-driven shell tricks make
    builds harder to read and harder to cache predictably with
    buildx. Three files have visible duplication in the early
    stages but auditable structure.
