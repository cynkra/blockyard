# Phase 3-7: Process Backend Core

Implement the `Backend` interface using bubblewrap (`bwrap`) for worker
sandboxing — no container runtime, no daemon, no socket. Workers are
spawned as sandboxed child processes with PID/mount/user namespace
isolation, seccomp filtering, and capability dropping. The process
backend targets deployments where startup latency matters (scale-to-zero,
internal-only) or where the Docker socket privilege is unacceptable.

This phase covers the core implementation: config, backend struct,
all twelve `Backend` methods (ten existing plus two added by this
phase: `Preflight` and `CleanupOrphanResources`), port and UID
allocation, bwrap argument construction, log capture, worker egress
preflight, and tests. Packaging and deployment artifacts (seccomp
profile, Dockerfile, release binaries) are deferred to phase 3-8.

Independent of the operations track (phases 3-2 through 3-5). Can be
developed in parallel with phase 3-6 (data mounts) and phase 3-9
(pre-fork).

---

## Prerequisites from Earlier Phases

- **Phase 3-1** — migration tooling and conventions. Phase 3-7 does
  not add migrations, but follows the same testing conventions.
- **Backend interface** (`internal/backend/backend.go`) — the ten-method
  `Backend` interface (`Spawn`, `Stop`, `HealthCheck`, `Logs`, `Addr`,
  `Build`, `ListManaged`, `RemoveResource`, `ContainerStats`,
  `UpdateResources`), along with `WorkerSpec`, `BuildSpec`, `BuildResult`,
  `LogStream`, `ManagedResource`, and `ErrNotSupported`, are stable.
  Phase 3-7 adds two new methods (`CleanupOrphanResources`, `Preflight`)
  and renames `ContainerStats` → `WorkerResourceUsage` (see deliverable
  #13), bringing the total to twelve.
- **`backends.md` design doc** — process backend design rationale,
  isolation properties, deployment modes, and comparison with Docker.
  Phase 3-7 implements the design described there.

No dependency on phases 3-2 through 3-5 (operations track) or
phase 3-6 (per-app config). The process backend uses the same
`WorkerSpec` fields as the Docker backend — new per-app fields from
phase 3-6 (`DataMounts`, `Image`, `Runtime`) are not consumed here and
can be integrated later.

## Deliverables

1. **`[process]` config section** (`internal/config/config.go`) —
   `ProcessConfig` struct with `bwrap_path`, `r_path`,
   `seccomp_profile`, `port_range_start`, `port_range_end`,
   `worker_uid_range_start`, `worker_uid_range_end`, `worker_gid`.
2. **Backend selection** (`cmd/blockyard/main.go`) — `[server] backend`
   field gains `"process"` as a valid value. Startup instantiates
   `ProcessBackend` or `DockerBackend` accordingly.
3. **Process backend struct** (`internal/backend/process/process.go`) —
   `ProcessBackend` implementing all twelve `Backend` interface methods.
   `UpdateResources` returns `backend.ErrNotSupported` since the process
   backend does not enforce per-worker resource limits (decision #6);
   `api/apps.go` skips the warning log when it sees `ErrNotSupported`
   from this call so app-update requests don't spam one noisy line per
   worker (see step 6).
4. **Port allocator** (`internal/backend/process/ports.go`) — allocates
   and releases localhost ports from a configured range.
5. **bwrap command construction** (`internal/backend/process/bwrap.go`)
   — builds the bwrap argument list from a `WorkerSpec`, including
   `--uid <N> --gid <G>` flags for the UID/GID egress isolation hooks
   (see #9 below).
6. **Log capture** (`internal/backend/process/logs.go`) — captures
   stdout/stderr from child processes and serves them via `LogStream`.
7. **Build support** — bwrap-sandboxed builds with write access to the
   build output directory. Same `BuildSpec` → R script → pak flow as
   the Docker backend.
8. **Preflight check** — verify `bwrap` is installed, user namespaces
   are enabled (`/proc/sys/kernel/unprivileged_userns_clone`), the
   port and UID ranges are sized correctly, resource-limit fields
   are not silently ignored, and worker egress is firewalled. The
   egress check spawns the blockyard binary in `probe` mode under a
   bwrap sandbox configured exactly like a real worker (same
   UID/GID/namespaces) and tries TCP-connecting to the cloud metadata
   endpoint plus configured Redis/OpenBao/database addresses. Any
   reachable target produces a warning (or error, for metadata).
9. **Worker UID/GID isolation** — each worker is allocated a unique
   host UID from `worker_uid_range_start..end` and shares a single
   `worker_gid` with all other workers. This is the foundation for
   the egress firewall (decision #5): operators install destination-
   scoped `iptables -m owner --gid-owner $worker_gid -d <internal-ip>
   -j REJECT` rules to block worker access to specific internal
   services (Redis, OpenBao, database, cloud metadata) without
   affecting blockyard itself and without blocking the open internet
   (workers can still reach external APIs, download data, etc.).
   Implemented via a `uidAllocator` (parallel to the port allocator)
   and `--uid <N> --gid <G>` flags in `bwrapArgs`. For the host UID
   mapping to actually take effect, blockyard must run as root or
   bwrap must be setuid — verified by `checkBwrapHostUIDMapping`
   (see deliverable #8).
10. **`blockyard probe` subcommand** — small TCP-connectivity probe
    used by `checkWorkerEgress`. Dispatched early in `main.go` based
    on `os.Args[1]`. ~30 lines, no external tools required, runs
    inside the same bwrap sandbox a worker uses.
11. **Tests** — unit tests for bwrap argument construction, port
    allocation, UID allocation, and log capture. Integration tests
    (tagged `process_test`) for spawn/health/stop lifecycle. Skipped
    when bwrap is unavailable.
12. **Runtime tab template — handle zero memory limit**
    (`internal/ui/templates/tab_runtime.html`). The current template
    renders `{{.MemoryUsageBytes | humanBytes}} / {{.MemoryLimitBytes | humanBytes}}`
    unconditionally. Process-backend workers have `MemoryLimitBytes = 0`
    (no per-worker cgroup limit — see decision #4), which would render
    as `"45 MB / 0 B"`. Wrap the limit in a conditional so it renders
    just `"45 MB"` when the limit is zero. This is backend-agnostic:
    Docker workers without a configured memory limit currently render
    the same way and benefit from the fix.
13. **Backend interface decoupling** — renames and new methods to remove
    Docker-specific assumptions from the interface and shared code:
    - Rename `ContainerStats` → `WorkerResourceUsage` and
      `ContainerStatsResult` → `WorkerResourceUsageResult` on the
      `Backend` interface (and all callers: `api/runtime.go`,
      `ui/sidebar.go`, `backend/mock/mock.go`, test stubs).
      Implementation order: do this rename as one atomic commit
      (interface + Docker backend + mock + all callers) *before*
      writing `ProcessBackend`. The new backend's compile-time
      interface check (`var _ backend.Backend = (*ProcessBackend)(nil)`)
      depends on the renamed method existing on the interface; doing
      it the other way around leaves the tree broken between commits.
    - Add `CleanupOrphanResources(ctx) error` to the `Backend`
      interface. Docker cleans iptables rules; process backend is a
      no-op. Removes the hard `docker` import from `ops/ops.go`.
    - Add `Preflight(ctx) (*preflight.Report, error)` to the `Backend`
      interface. Each backend checks its own prerequisites (Docker:
      socket/image/mounts; process: bwrap/R/userns/ports). `main.go`
      calls `be.Preflight()` instead of branching by backend type.
      **Move backend-specific check functions out of `internal/preflight`
      into the backend packages** to break the import cycle this would
      otherwise create. Currently `internal/preflight/docker_checks.go`
      imports `internal/backend/docker`; once `internal/backend`
      imports `internal/preflight` for the `Report` return type, that
      becomes `backend → preflight → backend/docker → backend`. The
      fix is to move the docker check functions into
      `internal/backend/docker/preflight.go` and the new process
      check functions into `internal/backend/process/preflight.go`.
      `internal/preflight` shrinks to a leaf package containing only
      `Report`, `Result`, severity constants, log formatting, and
      shared helpers (e.g. `addrs.go` for URL/DSN parsing). Both
      backend packages import it; it imports neither. No cycle.
    - Move `ParseMemoryLimit` from `internal/backend/docker` to
      `internal/units` — it's used by `api/apps.go` for input
      validation and has nothing to do with Docker.
    - Move `default_memory_limit` and `default_cpu_limit` from
      `[docker]` to `[server]` — these are worker resource defaults,
      not Docker concepts. The Docker backend enforces them via cgroup
      limits; a future k8s backend would translate them to Pod
      resource requests/limits. The process backend (decision #6)
      does not enforce per-worker limits — `process.RunPreflight`
      emits a warning when either field is set to a non-default
      value, so operators are not silently misled. The move is about
      config shape for backend-neutral fields, not a claim that every
      backend enforces them.
    - Move `store_retention` from `[docker]` to `[storage]` — it
      controls R library cache eviction, which is backend-neutral.
    - Rename `skip_docker_preflight` → `skip_preflight` on
      `ServerConfig`.

## Step-by-step

### Step 1: Config additions

Add `ProcessConfig` to `internal/config/config.go`:

```go
type ProcessConfig struct {
    BwrapPath         string `toml:"bwrap_path"`           // path to bubblewrap binary
    RPath             string `toml:"r_path"`                // path to R binary
    SeccompProfile    string `toml:"seccomp_profile"`       // path to custom seccomp JSON; empty = built-in
    PortRangeStart    int    `toml:"port_range_start"`      // first port for workers (inclusive)
    PortRangeEnd      int    `toml:"port_range_end"`        // last port for workers (inclusive)
    WorkerUIDStart    int    `toml:"worker_uid_range_start"` // first host UID for workers (inclusive)
    WorkerUIDEnd      int    `toml:"worker_uid_range_end"`   // last host UID for workers (inclusive)
    WorkerGID         int    `toml:"worker_gid"`            // shared host GID for all workers (used by egress firewall rules)
}
```

Add the field to `Config`:

```go
type Config struct {
    // ...existing fields...
    Process *ProcessConfig `toml:"process"` // nil when backend != "process"
}
```

Add `Backend` field to `ServerConfig` and move backend-neutral
worker defaults out of `DockerConfig`:

```go
type ServerConfig struct {
    // ...existing fields...
    Backend            string  `toml:"backend"`              // "docker" (default) or "process"
    SkipPreflight      bool    `toml:"skip_preflight"`       // skip backend-specific preflight checks at startup
    DefaultMemoryLimit string  `toml:"default_memory_limit"` // fallback memory limit for workers (e.g. "2g"); moved from [docker]
    DefaultCPULimit    float64 `toml:"default_cpu_limit"`    // fallback CPU limit (fractional vCPUs); moved from [docker]
}
```

Move `store_retention` from `DockerConfig` to `StorageConfig`:

```go
type StorageConfig struct {
    // ...existing fields...
    StoreRetention Duration `toml:"store_retention"` // moved from [docker]; 0 = disabled
}
```

Keep backward-compat TOML parsing for one release. The cleanest way to
do this with `BurntSushi/toml` is to leave the old fields on the
source structs under a deprecated-rename and copy them across in a
migration shim that runs after `toml.Unmarshal` and before
`applyDefaults`:

```go
type DockerConfig struct {
    // ...remaining fields...
    // Deprecated fields kept for one release so existing TOML still
    // parses. migrateDeprecatedFields copies these into their new
    // homes and warns. Remove in the next major bump.
    DeprecatedDefaultMemoryLimit string   `toml:"default_memory_limit"`
    DeprecatedDefaultCPULimit    float64  `toml:"default_cpu_limit"`
    DeprecatedStoreRetention     Duration `toml:"store_retention"`
}

type ServerConfig struct {
    // ...fields including the new Backend/SkipPreflight/DefaultMemoryLimit/DefaultCPULimit...
    DeprecatedSkipDockerPreflight bool `toml:"skip_docker_preflight"`
}
```

```go
// migrateDeprecatedFields copies old [docker]/[server] field values
// into their new locations when the new field is unset and the old
// field is present. Emits a deprecation warning for each move. Called
// once from Load(), between toml.Unmarshal and applyDefaults.
func migrateDeprecatedFields(cfg *Config) {
    if cfg.Server.DefaultMemoryLimit == "" && cfg.Docker.DeprecatedDefaultMemoryLimit != "" {
        cfg.Server.DefaultMemoryLimit = cfg.Docker.DeprecatedDefaultMemoryLimit
        slog.Warn("config: docker.default_memory_limit is deprecated; use server.default_memory_limit")
    }
    if cfg.Server.DefaultCPULimit == 0 && cfg.Docker.DeprecatedDefaultCPULimit != 0 {
        cfg.Server.DefaultCPULimit = cfg.Docker.DeprecatedDefaultCPULimit
        slog.Warn("config: docker.default_cpu_limit is deprecated; use server.default_cpu_limit")
    }
    if cfg.Storage.StoreRetention.Duration == 0 && cfg.Docker.DeprecatedStoreRetention.Duration != 0 {
        cfg.Storage.StoreRetention = cfg.Docker.DeprecatedStoreRetention
        slog.Warn("config: docker.store_retention is deprecated; use storage.store_retention")
    }
    if !cfg.Server.SkipPreflight && cfg.Server.DeprecatedSkipDockerPreflight {
        cfg.Server.SkipPreflight = true
        slog.Warn("config: server.skip_docker_preflight is deprecated; use server.skip_preflight")
    }
}
```

When both old and new fields are set, new wins and the old is dropped
silently — the operator is transitioning and already has the new value
they want. The deprecated fields are removed entirely in the next
release.

Defaults in `applyDefaults()`:

```go
if cfg.Server.Backend == "" {
    cfg.Server.Backend = "docker"
}
if cfg.Process != nil {
    processDefaults(cfg.Process)
}
```

```go
func processDefaults(c *ProcessConfig) {
    if c.BwrapPath == "" {
        c.BwrapPath = "/usr/bin/bwrap"
    }
    if c.RPath == "" {
        c.RPath = "/usr/bin/R"
    }
    if c.PortRangeStart == 0 {
        c.PortRangeStart = 10000
    }
    if c.PortRangeEnd == 0 {
        c.PortRangeEnd = 10999
    }
    if c.WorkerUIDStart == 0 {
        c.WorkerUIDStart = 60000
    }
    if c.WorkerUIDEnd == 0 {
        c.WorkerUIDEnd = 60999
    }
    if c.WorkerGID == 0 {
        c.WorkerGID = 65534 // nogroup
    }
}
```

Validation in `validate()`:

```go
switch cfg.Server.Backend {
case "docker":
    if cfg.Docker.Image == "" {
        return fmt.Errorf("config: docker.image must not be empty")
    }
case "process":
    if cfg.Process == nil {
        return fmt.Errorf("config: [process] section required when backend = \"process\"")
    }
    if cfg.Process.PortRangeEnd < cfg.Process.PortRangeStart {
        return fmt.Errorf("config: process.port_range_end must be >= port_range_start")
    }
    if cfg.Process.WorkerUIDEnd < cfg.Process.WorkerUIDStart {
        return fmt.Errorf("config: process.worker_uid_range_end must be >= worker_uid_range_start")
    }
    // Worker UID range size should accommodate peak worker count + headroom
    // for rolling updates (phase 3-8 runs old + new workers concurrently).
    uidCount := cfg.Process.WorkerUIDEnd - cfg.Process.WorkerUIDStart + 1
    portCount := cfg.Process.PortRangeEnd - cfg.Process.PortRangeStart + 1
    if uidCount < portCount {
        return fmt.Errorf("config: process.worker_uid_range must be at least as large as port_range (got %d UIDs vs %d ports)", uidCount, portCount)
    }
default:
    return fmt.Errorf("config: server.backend must be \"docker\" or \"process\", got %q", cfg.Server.Backend)
}
```

TOML example:

```toml
[server]
backend = "process"

[process]
bwrap_path = "/usr/bin/bwrap"
r_path = "/usr/bin/R"
seccomp_profile = ""
port_range_start = 10000
port_range_end = 10999
worker_uid_range_start = 60000
worker_uid_range_end = 60999
worker_gid = 65534
```

**Sizing the port range.** Each running worker consumes one port from
the range. The default range (10000-10999) provides 1000 ports — enough
for most deployments. Operators who plan to enable rolling updates
(phase 3-8) should size the range for ~2x peak worker count, since
both the old and new server allocate from the same range during the
overlap window. Running close to range capacity will block rolling
updates with a "no free ports" error.

**Worker UID range and GID.** Each running worker is assigned a unique
host UID from `worker_uid_range_start..worker_uid_range_end`, and all
workers share `worker_gid` as their host GID. This is the foundation
for egress isolation: blockyard runs as its own UID and can reach
Redis/database/OpenBao freely; workers run as different UIDs in a
shared GID, and operators install iptables rules that block worker
access to **specific internal services** (not blanket egress —
workers legitimately need to reach the open internet to download
data, call APIs, fetch models, etc.):

```sh
# Allow blockyard's own egress (to Redis, DB, OpenBao, etc.)
iptables -A OUTPUT -m owner --uid-owner blockyard -j ACCEPT
# Block worker access to specific internal destinations, not the
# open internet. The worker GID is the match; the destination
# address narrows the rule.
iptables -A OUTPUT -m owner --gid-owner 65534 -d 169.254.169.254 -j REJECT
iptables -A OUTPUT -m owner --gid-owner 65534 -d <redis-ip>     -j REJECT
iptables -A OUTPUT -m owner --gid-owner 65534 -d <openbao-ip>   -j REJECT
iptables -A OUTPUT -m owner --gid-owner 65534 -d <database-ip>  -j REJECT
# Traffic to the open internet falls through and is allowed.
```

The `process.RunPreflight` check `checkWorkerEgress` verifies these
rules are in place by spawning a probe under the worker UID/GID and
attempting TCP connections to the same internal endpoints (cloud
metadata, Redis, OpenBao, database). It does *not* probe the open
internet — workers are expected to reach it. See step 7 and
decision #5 for the threat model and limitations.

**Deployment modes and the UID mapping requirement.** The iptables
`--uid-owner`/`--gid-owner` match works on the process's *host*
UID/GID as the kernel sees it, not on the namespace-local UID inside
the sandbox. For bwrap's `--uid N --gid G` flags to actually produce
a host-visible UID/GID of N/G (so the iptables rules match), one of
the following must hold:

- **Blockyard runs as root** (typical containerized deployment, where
  blockyard is PID 1 root inside a container). bwrap inherits root
  and can set up any uid_map. This is the recommended mode and works
  with a distro-default bwrap.
- **bwrap is setuid root** on the host (`chmod u+s /usr/bin/bwrap`).
  This is the default on Fedora/RHEL but *not* on Debian 12+ or
  Ubuntu 24.04+, which ship bwrap relying on unprivileged user
  namespaces. On those distros a native non-root blockyard deployment
  needs an operator-installed setuid bwrap.

Running native non-root with an unprivileged bwrap produces a silent
failure mode: workers start fine but all run under blockyard's own
host UID/GID, so the per-worker isolation collapses and the iptables
rules match nothing. `process.RunPreflight` catches this explicitly
via `checkBwrapHostUIDMapping` (step 7) — it spawns bwrap with a
distinct sandbox UID and verifies the child's *host-side*
`/proc/<pid>/status` reports the requested UID, not the caller's UID.

The UID range must be at least as large as the port range, since each
worker consumes one port and one UID. Defaults: 60000-60999 (1000
UIDs) and GID 65534 (`nogroup`). Operators may prefer a dedicated
group like `blockyard-workers`; in that case set `worker_gid` to that
group's numeric ID.

Auto-construct from env vars (same pattern as other optional sections):

```go
if cfg.Process == nil && envPrefixExists("BLOCKYARD_PROCESS_") {
    cfg.Process = &ProcessConfig{}
    processDefaults(cfg.Process)
}
```

### Step 2: Port allocator

New file `internal/backend/process/ports.go`. The allocator manages a
range of localhost ports, handing out free ports on `Alloc()` and
returning them on `Release()`. Backed by a bitset for O(1) allocation.

```go
package process

import (
    "fmt"
    "net"
    "sync"
)

// portAllocator manages a fixed range of localhost ports for workers.
type portAllocator struct {
    mu    sync.Mutex
    start int
    used  []bool // index = port - start
}

func newPortAllocator(start, end int) *portAllocator {
    size := end - start + 1
    return &portAllocator{
        start: start,
        used:  make([]bool, size),
    }
}

// Alloc returns the next free port, or an error if all ports are in use.
// After marking a port as allocated in the bitset, it verifies the port
// is actually bindable (TCP listen + immediate close). This prevents
// TOCTOU failures where another process on the host has already bound
// the port. If the probe fails, the port is skipped and the scan
// continues to the next free slot.
func (p *portAllocator) Alloc() (int, error) {
    p.mu.Lock()
    defer p.mu.Unlock()
    for i, taken := range p.used {
        if !taken {
            port := p.start + i
            if !probePort(port) {
                continue // port in use by another process; skip
            }
            p.used[i] = true
            return port, nil
        }
    }
    return 0, fmt.Errorf("process backend: all %d ports in use", len(p.used))
}

// probePort attempts a TCP listen on 127.0.0.1:port to verify the port
// is available. Returns true if the listen succeeds (port is free).
func probePort(port int) bool {
    ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
    if err != nil {
        return false
    }
    ln.Close()
    return true
}

// Release returns a port to the pool. No-op if the port is out of range.
func (p *portAllocator) Release(port int) {
    p.mu.Lock()
    defer p.mu.Unlock()
    idx := port - p.start
    if idx >= 0 && idx < len(p.used) {
        p.used[idx] = false
    }
}

// InUse returns the number of currently allocated ports.
func (p *portAllocator) InUse() int {
    p.mu.Lock()
    defer p.mu.Unlock()
    n := 0
    for _, taken := range p.used {
        if taken {
            n++
        }
    }
    return n
}
```

### Step 2b: UID allocator

New file `internal/backend/process/uids.go`. Same shape as the port
allocator but without the bind probe — UIDs aren't a host resource we
need to verify; they're just identifiers for filesystem permissions
and iptables `--uid-owner` rules. The allocator is per-process state,
not coordinated across blockyard instances. Phase 3-8 (rolling
updates) must size the UID range for ~2x peak workers, same as the
port range.

```go
package process

import (
    "fmt"
    "sync"
)

// uidAllocator manages a fixed range of host UIDs for workers.
// Each running worker is assigned a unique UID; on exit the UID is
// returned to the pool. The allocator is in-memory only.
type uidAllocator struct {
    mu    sync.Mutex
    start int
    used  []bool // index = uid - start
}

func newUIDAllocator(start, end int) *uidAllocator {
    size := end - start + 1
    return &uidAllocator{
        start: start,
        used:  make([]bool, size),
    }
}

// Alloc returns the next free UID, or an error if all UIDs are in use.
func (u *uidAllocator) Alloc() (int, error) {
    u.mu.Lock()
    defer u.mu.Unlock()
    for i, taken := range u.used {
        if !taken {
            u.used[i] = true
            return u.start + i, nil
        }
    }
    return 0, fmt.Errorf("process backend: all %d worker UIDs in use", len(u.used))
}

// Release returns a UID to the pool. No-op if out of range.
func (u *uidAllocator) Release(uid int) {
    u.mu.Lock()
    defer u.mu.Unlock()
    idx := uid - u.start
    if idx >= 0 && idx < len(u.used) {
        u.used[idx] = false
    }
}
```

### Step 3: bwrap command construction

New file `internal/backend/process/bwrap.go`. Builds the bwrap
argument list from a `WorkerSpec` and `ProcessConfig`.

The filesystem strategy is `--ro-bind / /` — bind the entire host root
read-only, then mount over specific paths to provide writable scratch
space and app-specific content. bwrap processes arguments left-to-right,
so later mounts shadow earlier ones at the same path. This avoids
enumerating distro-specific paths (`/lib` vs `/usr/lib`, etc.) and
ensures every system file R needs is available without maintenance.

In containerized mode (recommended), the "root" is the outer
container's minimal filesystem — no secrets, no host data. In native
mode, more of the host is visible but read-only.

Writable paths are limited to:
- `/tmp` — tmpfs for R tempfiles and scratch
- Transfer directory — file upload/download (phase 2-7)

Everything else is read-only.

```go
package process

import (
    "fmt"
    "strconv"

    "github.com/cynkra/blockyard/internal/backend"
    "github.com/cynkra/blockyard/internal/config"
)

// bwrapArgs constructs the bwrap command-line arguments for a worker.
// uid is the host UID this worker runs as (allocated from the worker
// UID pool); gid is the shared host GID for all workers (used by the
// operator's destination-scoped egress firewall rules). Together they
// let operators install rules like
// `iptables -m owner --gid-owner $worker_gid -d <internal-ip> -j REJECT`
// to block worker access to specific internal services without
// affecting blockyard or blocking the open internet.
//
// For the host UID/GID to actually take effect (so iptables owner
// match works), blockyard must run as root or bwrap must be setuid.
// Verified at startup by checkBwrapHostUIDMapping.
func bwrapArgs(cfg *config.ProcessConfig, spec backend.WorkerSpec, port, uid, gid int) []string {
    args := []string{
        // Namespace isolation
        "--unshare-pid",
        "--unshare-user",
        "--unshare-uts",

        // Host identity — workers run as a per-worker UID and a shared
        // GID. The UID gives per-worker filesystem isolation; the GID
        // is the target of the operator's egress firewall rule.
        "--uid", strconv.Itoa(uid),
        "--gid", strconv.Itoa(gid),

        // Process lifecycle
        "--die-with-parent",
        "--new-session",

        // Filesystem: read-only bind of the entire host root.
        // In containerized mode this is the outer container's rootfs.
        // In native mode this is the host filesystem (read-only).
        "--ro-bind", "/", "/",

        // Writable scratch space (shadows the read-only /tmp).
        "--tmpfs", "/tmp",

        // Virtual filesystems (shadow the read-only copies).
        "--proc", "/proc",
        "--dev", "/dev",

        // Working directory — /tmp is always writable (tmpfs above).
        // Without --chdir the inherited cwd may not be accessible
        // after --unshare-user remaps the UID.
        "--chdir", "/tmp",

        // App bundle — shadow with the specific bundle path.
        "--ro-bind", spec.BundlePath, spec.WorkerMount,
    }

    // R library (read-only) — mount target must match the Docker
    // backend's convention so the same R_LIBS env var resolves
    // correctly on either backend. Store-assembled library (phase 2-6)
    // mounts at /blockyard-lib-store; legacy per-bundle library
    // (phase 2-5) mounts at /blockyard-lib. Must not use /lib, which
    // shadows the system shared library directory.
    if spec.LibDir != "" {
        args = append(args, "--ro-bind", spec.LibDir, "/blockyard-lib-store")
    } else if spec.LibraryPath != "" {
        args = append(args, "--ro-bind", spec.LibraryPath, "/blockyard-lib")
    }

    // Worker token directory (read-only, optional) — mount target
    // /var/run/blockyard matches the Docker backend's convention.
    // Workers read /var/run/blockyard/token to authenticate to the
    // packages endpoint.
    if spec.TokenDir != "" {
        args = append(args, "--ro-bind", spec.TokenDir, "/var/run/blockyard")
    }

    // Transfer directory (read-write, optional) — mount target /transfer
    // matches the Docker backend's convention. Workers read the handoff
    // file via the BLOCKYARD_TRANSFER_PATH env var (set to
    // "/transfer/board.json" in server/transfer.go).
    if spec.TransferDir != "" {
        args = append(args, "--bind", spec.TransferDir, "/transfer")
    }

    // Capability dropping — bwrap drops all by default with --unshare-user,
    // but we explicitly drop to be defensive in case of flag changes.
    args = append(args, "--cap-drop", "ALL")

    // Separator and command
    args = append(args, "--")
    if len(spec.Cmd) > 0 {
        args = append(args, spec.Cmd...)
    } else {
        args = append(args,
            cfg.RPath, "-e",
            fmt.Sprintf("shiny::runApp('%s', port=%s, host='127.0.0.1')",
                spec.WorkerMount, strconv.Itoa(port)),
        )
    }

    return args
}

// bwrapBuildArgs constructs the bwrap arguments for a build task.
// Same root strategy as workers but with additional writable mounts
// for build output. uid/gid follow the same convention as bwrapArgs;
// builds use the next free worker UID and the same shared GID, so
// build egress is also covered by the operator's firewall rule.
func bwrapBuildArgs(cfg *config.ProcessConfig, spec backend.BuildSpec, uid, gid int) []string {
    args := []string{
        "--unshare-pid",
        "--unshare-user",
        "--unshare-uts",

        "--uid", strconv.Itoa(uid),
        "--gid", strconv.Itoa(gid),

        "--die-with-parent",
        "--new-session",

        "--ro-bind", "/", "/",
        "--tmpfs", "/tmp",
        "--proc", "/proc",
        "--dev", "/dev",
        "--chdir", "/tmp",
    }

    // Build mounts — shadow specific paths with read-only or read-write
    // binds as needed. Read-write mounts (e.g., library output dir)
    // shadow the read-only root at that path.
    for _, m := range spec.Mounts {
        if m.ReadOnly {
            args = append(args, "--ro-bind", m.Source, m.Target)
        } else {
            args = append(args, "--bind", m.Source, m.Target)
        }
    }

    args = append(args, "--cap-drop", "ALL")
    args = append(args, "--")
    args = append(args, spec.Cmd...)

    return args
}
```

Seccomp is handled separately from argument construction because
bwrap's `--seccomp <fd>` flag takes a file descriptor number, not a
file path. The caller must open the compiled BPF profile, pass the fd
to bwrap, and close it after the process starts. This is encapsulated
in a helper:

```go
// applySeccomp opens the seccomp BPF profile and configures cmd to pass
// it via an inherited fd. bwrap's --seccomp flag expects a file descriptor
// number, not a path. The profile must be pre-compiled to BPF binary
// format (not the Docker/OCI JSON format). Phase 3-8 ships the compiled
// profile; this phase accepts it as a pre-compiled file.
//
// Returns the args to prepend before "--" in the bwrap command line.
func applySeccomp(cmd *exec.Cmd, profilePath string) ([]string, error) {
    if profilePath == "" {
        return nil, nil
    }
    f, err := os.Open(profilePath)
    if err != nil {
        return nil, fmt.Errorf("open seccomp profile: %w", err)
    }
    // cmd.ExtraFiles[0] becomes fd 3 in the child.
    cmd.ExtraFiles = append(cmd.ExtraFiles, f)
    fd := 3 + len(cmd.ExtraFiles) - 1
    return []string{"--seccomp", strconv.Itoa(fd)}, nil
}

// spliceBeforeSeparator inserts extra into cmd.Args just before the
// "--" separator. cmd.Args[0] is the program name (set by exec.Command).
func spliceBeforeSeparator(cmd *exec.Cmd, extra []string) {
    for i, arg := range cmd.Args {
        if arg == "--" {
            result := make([]string, 0, len(cmd.Args)+len(extra))
            result = append(result, cmd.Args[:i]...)
            result = append(result, extra...)
            result = append(result, cmd.Args[i:]...)
            cmd.Args = result
            return
        }
    }
    // No separator found — append before the end (shouldn't happen
    // with well-formed bwrap args, but don't panic).
    cmd.Args = append(cmd.Args, extra...)
}
```

The `Spawn` and `Build` methods call `applySeccomp` after creating the
`exec.Cmd`, then `spliceBeforeSeparator` inserts the `--seccomp <fd>`
flags before `--` in `cmd.Args`. The seccomp profile file is closed by
the OS when the child process execs (Go sets `O_CLOEXEC` by default,
but bwrap reads the fd before exec). When `SeccompProfile` is empty,
`applySeccomp` returns nil and the splice is skipped — seccomp is
optional until phase 3-8 ships the compiled BPF profile.

### Step 4: Log capture

New file `internal/backend/process/logs.go`. Each worker's stdout and
stderr are captured to ring buffers. The `Logs()` method creates a
`LogStream` that replays buffered lines and then follows new output.

```go
package process

import (
    "bufio"
    "io"
    "sync"

    "github.com/cynkra/blockyard/internal/backend"
)

// logBuffer captures output from a child process and serves it as
// a LogStream. Lines are stored in a fixed-size circular buffer.
// Subscribers track a global sequence number so their cursor stays
// valid across ring wraps. Each subscriber gets its own notification
// channel so broadcasts wake all viewers, not just one.
type logBuffer struct {
    mu     sync.Mutex
    buf    []string         // fixed-size ring buffer
    size   int              // len(buf), set at init
    seq    uint64           // total lines written (monotonic); buf index = seq % size
    closed bool
    subs   []chan struct{}   // per-subscriber notification channels
}

func newLogBuffer(maxLines int) *logBuffer {
    return &logBuffer{
        buf:  make([]string, maxLines),
        size: maxLines,
    }
}

// broadcast wakes all subscribers. Called with lb.mu held.
func (lb *logBuffer) broadcast() {
    for _, ch := range lb.subs {
        select {
        case ch <- struct{}{}:
        default:
        }
    }
}

// subscribe registers a notification channel. Returns an unsubscribe func.
func (lb *logBuffer) subscribe() (ch chan struct{}, unsub func()) {
    ch = make(chan struct{}, 1)
    lb.mu.Lock()
    lb.subs = append(lb.subs, ch)
    lb.mu.Unlock()
    return ch, func() {
        lb.mu.Lock()
        for i, c := range lb.subs {
            if c == ch {
                lb.subs = append(lb.subs[:i], lb.subs[i+1:]...)
                break
            }
        }
        lb.mu.Unlock()
    }
}

// ingest reads lines from r until EOF and writes them to the ring.
func (lb *logBuffer) ingest(r io.Reader) {
    scanner := bufio.NewScanner(r)
    for scanner.Scan() {
        lb.mu.Lock()
        lb.buf[lb.seq%uint64(lb.size)] = scanner.Text()
        lb.seq++
        lb.broadcast()
        lb.mu.Unlock()
    }
    lb.mu.Lock()
    lb.closed = true
    lb.broadcast()
    lb.mu.Unlock()
}

// stream returns a LogStream that replays buffered lines and follows.
func (lb *logBuffer) stream() backend.LogStream {
    ch := make(chan string, 64)
    done := make(chan struct{})
    notify, unsub := lb.subscribe()

    go func() {
        defer close(ch)
        defer unsub()

        // Start cursor at the oldest available line. If the ring has
        // wrapped, that's seq - size; otherwise 0.
        lb.mu.Lock()
        var cursor uint64
        if lb.seq > uint64(lb.size) {
            cursor = lb.seq - uint64(lb.size)
        }
        lb.mu.Unlock()

        for {
            lb.mu.Lock()
            seq := lb.seq
            closed := lb.closed
            // Copy out any lines between cursor and current seq.
            var pending []string
            for cursor < seq {
                // The line at global sequence number `cursor` lives at
                // buf[cursor % size] — but only if it hasn't been
                // overwritten (cursor >= seq - size).
                if cursor >= seq-uint64(lb.size) {
                    pending = append(pending, lb.buf[cursor%uint64(lb.size)])
                }
                cursor++
            }
            lb.mu.Unlock()

            for _, line := range pending {
                select {
                case ch <- line:
                case <-done:
                    return
                }
            }
            if closed && cursor >= seq {
                return
            }
            // Wait for new data.
            select {
            case <-notify:
            case <-done:
                return
            }
        }
    }()

    return backend.LogStream{
        Lines: ch,
        Close: func() { close(done) },
    }
}
```

### Step 5: Process backend struct and lifecycle

New file `internal/backend/process/process.go`. The core backend
implementation. Each worker is tracked by its PID, port, and log buffer.
The backend uses `os/exec` to launch bwrap and `os.Process.Signal` /
`os.Process.Wait` for lifecycle management.

```go
package process

import (
    "bufio"
    "context"
    "fmt"
    "io"
    "log/slog"
    "net"
    "os"
    "os/exec"
    "runtime"
    "strconv"
    "strings"
    "sync"
    "syscall"
    "time"

    "github.com/cynkra/blockyard/internal/backend"
    "github.com/cynkra/blockyard/internal/config"
    "github.com/cynkra/blockyard/internal/preflight"
)

// Compile-time interface check.
var _ backend.Backend = (*ProcessBackend)(nil)

// cpuSample stores a previous CPU reading for delta computation.
type cpuSample struct {
    ticks uint64    // utime + stime in clock ticks
    when  time.Time
}

// workerProc holds per-worker state.
type workerProc struct {
    cmd      *exec.Cmd
    process  *os.Process
    port     int
    uid      int           // host UID this worker runs as (returned to pool on exit)
    spec     backend.WorkerSpec
    logs     *logBuffer
    done     chan struct{} // closed when process exits
    lastCPU  *cpuSample   // previous CPU sample for delta; nil on first call
}

// ProcessBackend implements backend.Backend using bubblewrap.
type ProcessBackend struct {
    cfg     *config.ProcessConfig // shortcut for fullCfg.Process; used in hot paths
    fullCfg *config.Config        // held for Preflight() — needs Redis/OpenBao/DB addrs for egress probe and Server.DefaultMemoryLimit/CPULimit for the resource-limit warning
    ports   *portAllocator
    uids    *uidAllocator

    mu      sync.Mutex
    workers map[string]*workerProc // keyed by worker ID
}

// New creates a ProcessBackend. Verifies that bwrap exists at the
// configured path. The full config is stored so Preflight() can read
// the addresses of Redis/OpenBao/database for the egress probe and
// the server-level resource-limit fields for the warning check.
func New(fullCfg *config.Config) (*ProcessBackend, error) {
    cfg := fullCfg.Process
    if _, err := exec.LookPath(cfg.BwrapPath); err != nil {
        return nil, fmt.Errorf("process backend: bwrap not found at %q: %w",
            cfg.BwrapPath, err)
    }
    return &ProcessBackend{
        cfg:     cfg,
        fullCfg: fullCfg,
        ports:   newPortAllocator(cfg.PortRangeStart, cfg.PortRangeEnd),
        uids:    newUIDAllocator(cfg.WorkerUIDStart, cfg.WorkerUIDEnd),
        workers: make(map[string]*workerProc),
    }, nil
}

// Preflight implements backend.Backend. The check functions live in
// the same package (this file's siblings) — see preflight.go for the
// implementations. Returning the report through the interface lets
// main.go run the preflight without knowing which backend is active.
func (b *ProcessBackend) Preflight(_ context.Context) (*preflight.Report, error) {
    return RunPreflight(b.cfg, b.fullCfg), nil
}

func (b *ProcessBackend) Spawn(ctx context.Context, spec backend.WorkerSpec) error {
    // Warn when per-app resource limits are set: the process backend
    // does not enforce them (decision #6). checkResourceLimits only
    // catches the [server] defaults, so without this warning an app
    // with `memory_limit = "512m"` in the database would silently get
    // unlimited workers with no trace in the logs.
    if spec.MemoryLimit != "" || spec.CPULimit > 0 {
        slog.Warn("process backend: per-worker resource limits are ignored",
            "worker_id", spec.WorkerID,
            "app_id", spec.AppID,
            "memory_limit", spec.MemoryLimit,
            "cpu_limit", spec.CPULimit)
    }

    port, err := b.ports.Alloc()
    if err != nil {
        return err
    }
    uid, err := b.uids.Alloc()
    if err != nil {
        b.ports.Release(port)
        return err
    }

    args := bwrapArgs(b.cfg, spec, port, uid, b.cfg.WorkerGID)

    // exec.Command, not exec.CommandContext — the ctx passed to Spawn is
    // typically a request context that cancels when the handler returns.
    // CommandContext would SIGKILL the worker on cancellation. Worker
    // lifecycle is managed by Stop() and --die-with-parent, not by ctx.
    cmd := exec.Command(b.cfg.BwrapPath, args...) //nolint:gosec // G204: args from validated config

    // Kill bwrap if the blockyard server dies. --die-with-parent inside
    // bwrap only kills R when bwrap exits — it does NOT kill bwrap when
    // blockyard exits. Without Pdeathsig on the bwrap process itself,
    // a server crash would leave orphaned bwrap+R processes.
    cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

    // Helper to release both port and UID on error.
    releaseSlots := func() {
        b.ports.Release(port)
        b.uids.Release(uid)
    }

    // Seccomp — splice --seccomp <fd> args before the "--" separator.
    // No-op when SeccompProfile is empty (phase 3-8 ships the profile).
    if secArgs, err := applySeccomp(cmd, b.cfg.SeccompProfile); err != nil {
        releaseSlots()
        return fmt.Errorf("process backend: seccomp: %w", err)
    } else if len(secArgs) > 0 {
        spliceBeforeSeparator(cmd, secArgs)
    }

    // Minimal environment — do NOT inherit the server's env, which
    // contains database URLs, Redis credentials, OpenBao tokens, etc.
    //
    // R_LIBS must point at the mount target for the R library (see
    // bwrapArgs) and SHINY_PORT must be the allocated host port —
    // call sites pass a Cmd that reads `Sys.getenv('SHINY_PORT')`
    // to decide what port Shiny binds to. Both must match the Docker
    // backend's conventions so the same spec.Cmd works on either
    // backend without modification.
    rLibs := "/blockyard-lib"
    if spec.LibDir != "" {
        rLibs = "/blockyard-lib-store"
    }
    cmd.Env = []string{
        "PATH=/usr/bin:/usr/local/bin:/bin",
        "HOME=/tmp",
        "TMPDIR=/tmp",
        "LANG=C.UTF-8",
        "R_LIBS=" + rLibs,
    }
    for k, v := range spec.Env {
        cmd.Env = append(cmd.Env, k+"="+v)
    }
    cmd.Env = append(cmd.Env, fmt.Sprintf("SHINY_PORT=%d", port))

    // Log capture
    logs := newLogBuffer(10000)

    stdout, err := cmd.StdoutPipe()
    if err != nil {
        releaseSlots()
        return fmt.Errorf("process backend: stdout pipe: %w", err)
    }
    stderr, err := cmd.StderrPipe()
    if err != nil {
        releaseSlots()
        return fmt.Errorf("process backend: stderr pipe: %w", err)
    }

    // Pin this goroutine to its OS thread across the fork. Pdeathsig
    // fires when the *thread* that forked the child exits — without
    // LockOSThread the Go runtime may retire the thread and trigger
    // a spurious SIGKILL to bwrap.
    runtime.LockOSThread()
    err = cmd.Start()
    runtime.UnlockOSThread()
    if err != nil {
        releaseSlots()
        return fmt.Errorf("process backend: start bwrap: %w", err)
    }

    // Ingest stdout and stderr concurrently into the shared log buffer.
    // Two goroutines, not io.MultiReader — MultiReader reads sequentially
    // (stdout to EOF before stderr), which would suppress stderr for the
    // entire worker lifetime.
    go logs.ingest(stdout)
    go logs.ingest(stderr)

    done := make(chan struct{})
    go func() {
        _ = cmd.Wait()
        close(done)
        b.mu.Lock()
        delete(b.workers, spec.WorkerID)
        b.mu.Unlock()
        b.ports.Release(port)
        b.uids.Release(uid)
        slog.Info("process backend: worker exited",
            "worker_id", spec.WorkerID, "port", port, "uid", uid)
    }()

    b.mu.Lock()
    b.workers[spec.WorkerID] = &workerProc{
        cmd:     cmd,
        process: cmd.Process,
        port:    port,
        uid:     uid,
        spec:    spec,
        logs:    logs,
        done:    done,
    }
    b.mu.Unlock()

    slog.Info("process backend: spawned worker",
        "worker_id", spec.WorkerID,
        "port", port,
        "uid", uid,
        "pid", cmd.Process.Pid)
    return nil
}

func (b *ProcessBackend) Stop(_ context.Context, id string) error {
    b.mu.Lock()
    w, ok := b.workers[id]
    b.mu.Unlock()
    if !ok {
        return fmt.Errorf("process backend: worker %q not found", id)
    }

    // SIGTERM, then wait up to 10s, then SIGKILL.
    _ = w.process.Signal(syscall.SIGTERM)

    select {
    case <-w.done:
        return nil
    case <-time.After(10 * time.Second):
    }

    slog.Warn("process backend: worker did not exit after SIGTERM, sending SIGKILL",
        "worker_id", id)
    _ = w.process.Kill()
    <-w.done
    return nil
}

func (b *ProcessBackend) HealthCheck(ctx context.Context, id string) bool {
    b.mu.Lock()
    w, ok := b.workers[id]
    b.mu.Unlock()
    if !ok {
        return false
    }

    // Check if process is still running.
    select {
    case <-w.done:
        return false
    default:
    }

    // TCP probe to localhost:port.
    addr := fmt.Sprintf("127.0.0.1:%d", w.port)
    d := net.Dialer{Timeout: 2 * time.Second}
    conn, err := d.DialContext(ctx, "tcp", addr)
    if err != nil {
        return false
    }
    conn.Close()
    return true
}

func (b *ProcessBackend) Logs(_ context.Context, id string) (backend.LogStream, error) {
    b.mu.Lock()
    w, ok := b.workers[id]
    b.mu.Unlock()
    if !ok {
        return backend.LogStream{}, fmt.Errorf("process backend: worker %q not found", id)
    }
    return w.logs.stream(), nil
}

func (b *ProcessBackend) Addr(_ context.Context, id string) (string, error) {
    b.mu.Lock()
    w, ok := b.workers[id]
    b.mu.Unlock()
    if !ok {
        return "", fmt.Errorf("process backend: worker %q not found", id)
    }
    return fmt.Sprintf("127.0.0.1:%d", w.port), nil
}

func (b *ProcessBackend) Build(ctx context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
    // Builds run under the same UID pool as workers — they're sandboxed
    // R processes that share the worker firewall rules.
    uid, err := b.uids.Alloc()
    if err != nil {
        return backend.BuildResult{Success: false, ExitCode: 1, Logs: err.Error()}, nil
    }
    defer b.uids.Release(uid)

    args := bwrapBuildArgs(b.cfg, spec, uid, b.cfg.WorkerGID)
    // Context is appropriate here — builds are bounded, run-to-completion
    // tasks. If the caller cancels, the build should stop.
    cmd := exec.CommandContext(ctx, b.cfg.BwrapPath, args...) //nolint:gosec // G204: args from validated config
    cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

    // Seccomp — same splice as Spawn.
    if secArgs, err := applySeccomp(cmd, b.cfg.SeccompProfile); err != nil {
        return backend.BuildResult{Success: false, ExitCode: 1, Logs: err.Error()}, nil
    } else if len(secArgs) > 0 {
        spliceBeforeSeparator(cmd, secArgs)
    }

    // Minimal env + build-specific vars from spec.Env ([]string KEY=VALUE).
    cmd.Env = []string{
        "PATH=/usr/bin:/usr/local/bin:/bin",
        "HOME=/tmp",
        "TMPDIR=/tmp",
        "LANG=C.UTF-8",
    }
    cmd.Env = append(cmd.Env, spec.Env...)

    // Stream stdout/stderr line-by-line: pak builds run for minutes and
    // the build UI renders spec.LogWriter output live. Collecting with
    // CombinedOutput and replaying after exit would leave the UI stuck
    // on "building..." until completion and then dump everything at once.
    // The Docker backend streams via ContainerLogs Follow + scanner for
    // the same reason (docker.go Build step 5).
    stdout, err := cmd.StdoutPipe()
    if err != nil {
        return backend.BuildResult{Success: false, ExitCode: 1, Logs: err.Error()}, nil
    }
    stderr, err := cmd.StderrPipe()
    if err != nil {
        return backend.BuildResult{Success: false, ExitCode: 1, Logs: err.Error()}, nil
    }
    if err := cmd.Start(); err != nil {
        return backend.BuildResult{Success: false, ExitCode: 1, Logs: err.Error()}, nil
    }

    // Two goroutines, one per stream — sequential reads would suppress
    // stderr until stdout closes. A mutex serializes writes into the
    // shared logs buffer and the LogWriter callback so lines are not
    // interleaved mid-character. Interleave at *line* granularity is
    // fine; R/Shiny mix diagnostics across both streams and the Docker
    // backend merges them the same way.
    var (
        logsBuf strings.Builder
        logMu   sync.Mutex
        wg      sync.WaitGroup
    )
    ingest := func(r io.Reader) {
        defer wg.Done()
        sc := bufio.NewScanner(r)
        for sc.Scan() {
            line := sc.Text()
            logMu.Lock()
            logsBuf.WriteString(line)
            logsBuf.WriteByte('\n')
            if spec.LogWriter != nil {
                spec.LogWriter(line)
            }
            logMu.Unlock()
        }
    }
    wg.Add(2)
    go ingest(stdout)
    go ingest(stderr)
    wg.Wait()

    err = cmd.Wait()
    logs := logsBuf.String()

    if err != nil {
        exitCode := 1
        if exitErr, ok := err.(*exec.ExitError); ok {
            exitCode = exitErr.ExitCode()
        }
        return backend.BuildResult{
            Success:  false,
            ExitCode: exitCode,
            Logs:     logs,
        }, nil
    }
    return backend.BuildResult{
        Success:  true,
        ExitCode: 0,
        Logs:     logs,
    }, nil
}

func (b *ProcessBackend) ListManaged(_ context.Context) ([]backend.ManagedResource, error) {
    b.mu.Lock()
    defer b.mu.Unlock()
    var resources []backend.ManagedResource
    for id, w := range b.workers {
        resources = append(resources, backend.ManagedResource{
            ID:     id,
            Kind:   backend.ResourceContainer, // reuse Container kind for processes
            Labels: w.spec.Labels,
        })
    }
    return resources, nil
}

func (b *ProcessBackend) RemoveResource(_ context.Context, r backend.ManagedResource) error {
    b.mu.Lock()
    w, ok := b.workers[r.ID]
    b.mu.Unlock()
    if !ok {
        return nil // already gone
    }
    _ = w.process.Kill()
    <-w.done
    return nil
}

func (b *ProcessBackend) WorkerResourceUsage(_ context.Context, id string) (*backend.WorkerResourceUsageResult, error) {
    b.mu.Lock()
    w, ok := b.workers[id]
    b.mu.Unlock()
    if !ok {
        return nil, nil
    }

    rssBytes, ticks := readProcTreeStats(w.process.Pid)
    if rssBytes == 0 && ticks == 0 {
        return nil, nil // process exited between lookup and read
    }

    now := time.Now()
    clockTick := uint64(100) // sysconf(_SC_CLK_TCK), 100 on Linux

    // Compute CPU percentage from delta ticks / delta wall time,
    // matching the Docker backend's semantics. First call returns 0%.
    var cpuPercent float64
    b.mu.Lock()
    if w.lastCPU != nil {
        dt := now.Sub(w.lastCPU.when).Seconds()
        if dt > 0 {
            deltaTicks := float64(ticks - w.lastCPU.ticks)
            cpuPercent = (deltaTicks / float64(clockTick) / dt) * 100.0
        }
    }
    w.lastCPU = &cpuSample{ticks: ticks, when: now}
    b.mu.Unlock()

    return &backend.WorkerResourceUsageResult{
        CPUPercent:       cpuPercent,
        MemoryUsageBytes: rssBytes,
        MemoryLimitBytes: 0, // no per-worker cgroup limit
    }, nil
}

// UpdateResources is not supported by the process backend: we do not
// enforce per-worker cgroup limits (decision #6), so there is nothing
// to update. Returning ErrNotSupported lets callers distinguish "cannot
// do this" from "ran into a real error"; api/apps.go checks for
// ErrNotSupported and skips its warning log so app-update requests do
// not spam one noisy warning per worker.
func (b *ProcessBackend) UpdateResources(_ context.Context, _ string, _ int64, _ int64) error {
    return backend.ErrNotSupported
}

// readProcTreeStats aggregates RSS and CPU ticks across a process and
// all its descendants. cmd.Process.Pid is the bwrap process; the actual
// R process is a child of bwrap. Reading only the bwrap PID would show
// trivial resource usage (bwrap is a tiny C program). We walk
// /proc/{pid}/task/{tid}/children recursively to find all descendants
// and sum their stats.
//
// Returns (totalRSSBytes, totalTicks). Both zero means the process tree
// exited between lookup and read.
func readProcTreeStats(pid int) (rssBytes uint64, ticks uint64) {
    pids := collectDescendants(pid)
    pids = append([]int{pid}, pids...)

    var totalRSSKB uint64
    var totalUtime, totalStime uint64

    for _, p := range pids {
        rss, ut, st := readOneProcStats(p)
        totalRSSKB += rss
        totalUtime += ut
        totalStime += st
    }

    return totalRSSKB * 1024, totalUtime + totalStime
}

// collectDescendants returns all descendant PIDs of pid by walking
// /proc/{pid}/task/{tid}/children recursively.
func collectDescendants(pid int) []int {
    var result []int
    childrenPath := fmt.Sprintf("/proc/%d/task/%d/children", pid, pid)
    data, err := os.ReadFile(childrenPath)
    if err != nil {
        return nil
    }
    for _, field := range strings.Fields(string(data)) {
        child, err := strconv.Atoi(field)
        if err != nil {
            continue
        }
        result = append(result, child)
        result = append(result, collectDescendants(child)...)
    }
    return result
}

// readOneProcStats reads VmRSS, utime, and stime for a single PID.
// Returns zeros if the process is gone.
func readOneProcStats(pid int) (rssKB, utime, stime uint64) {
    // RSS from /proc/{pid}/status (VmRSS line, in kB).
    statusData, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
    if err != nil {
        return 0, 0, 0
    }
    for _, line := range splitLines(string(statusData)) {
        if strings.HasPrefix(line, "VmRSS:") {
            fields := strings.Fields(line)
            if len(fields) >= 2 {
                rssKB, _ = strconv.ParseUint(fields[1], 10, 64)
            }
            break
        }
    }

    // CPU from /proc/{pid}/stat (fields 14+15: utime + stime in clock ticks).
    statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
    if err != nil {
        return rssKB, 0, 0
    }
    statStr := string(statData)
    commEnd := strings.LastIndex(statStr, ")")
    if commEnd < 0 || commEnd+2 >= len(statStr) {
        return rssKB, 0, 0
    }
    fields := strings.Fields(statStr[commEnd+2:])
    // fields[0]=state, [1..]=field4+. utime=field14 → index 11, stime=field15 → index 12.
    if len(fields) > 12 {
        utime, _ = strconv.ParseUint(fields[11], 10, 64)
        stime, _ = strconv.ParseUint(fields[12], 10, 64)
    }
    return rssKB, utime, stime
}

// splitLines splits s into non-empty lines.
func splitLines(s string) []string {
    var lines []string
    start := 0
    for i := range len(s) {
        if s[i] == '\n' {
            line := s[start:i]
            if len(line) > 0 {
                lines = append(lines, line)
            }
            start = i + 1
        }
    }
    if start < len(s) {
        lines = append(lines, s[start:])
    }
    return lines
}
```

### Step 6: Backend selection at startup

Update `cmd/blockyard/main.go` to select the backend based on config:

```go
// Initialize backend
var be backend.Backend
switch cfg.Server.Backend {
case "process":
    be, err = process.New(cfg)
    if err != nil {
        slog.Error("failed to create process backend", "error", err)
        os.Exit(1)
    }
default: // "docker"
    be, err = docker.New(context.Background(), cfg, cfg.Storage.BundleServerPath)
    if err != nil {
        slog.Error("failed to create docker backend", "error", err)
        os.Exit(1)
    }
}
```

Add the import:

```go
import "github.com/cynkra/blockyard/internal/backend/process"
```

**`docker.New()` signature change.** The current signature takes
`*config.DockerConfig`; after moving `default_memory_limit` and
`default_cpu_limit` from `DockerConfig` to `ServerConfig` the backend
needs to read both sections, so `New()` takes the full `*config.Config`
(parallel to `process.New`). Inside the backend, the existing
`d.config.DefaultMemoryLimit` / `d.config.DefaultCPULimit` reads at
`docker.go:450-458` become `d.fullCfg.Server.DefaultMemoryLimit` /
`d.fullCfg.Server.DefaultCPULimit`. The `DockerBackend` struct gains a
`fullCfg *config.Config` field alongside the existing `config
*config.DockerConfig` shortcut; `New()` sets both from its argument.

The `docker.Image` validation in `validate()` moves into the `case
"docker"` branch (step 1) so it does not reject process-backend configs
that leave `docker.image` empty.

**Preflight via Backend interface.** Replace the current
`SkipDockerPreflight` → `RunDockerChecks` flow with a backend method:

```go
if !cfg.Server.SkipPreflight {
    preflightReport, err := be.Preflight(ctx)
    // ...log, check HasErrors()
}
```

Each backend implements `Preflight(ctx) (*preflight.Report, error)`:
- Docker: the existing check logic (socket, image pull, mount
  detection, builder check), moved from `internal/preflight` into
  `internal/backend/docker/preflight.go`.
- Process: the new check logic (bwrap, R, userns, port range,
  resource-limit warning, worker egress probe), in
  `internal/backend/process/preflight.go`.

This removes the backend-type branching from `main.go` and ensures
future backends (k8s) only need to implement the interface method.

**Store retention sweeper.** The current code gates on
`cfg.Docker.StoreRetention`. After the config move:

```go
if cfg.Storage.StoreRetention.Duration > 0 {
    pkgstore.SpawnEvictionSweeper(bgCtx, srv.PkgStore, cfg.Storage.StoreRetention.Duration)
}
```

**Startup cleanup.** Replace the direct `docker.CleanupOrphanMetadataRules()`
call in `ops.StartupCleanup` with:

```go
if err := srv.Backend.CleanupOrphanResources(ctx); err != nil {
    slog.Warn("startup: orphan resource cleanup failed", "error", err)
}
```

This removes the `internal/backend/docker` import from `ops/ops.go`.

**System checker.** Rename `DockerPing` → `BackendPing` in
`preflight.RuntimeDeps`.

### Step 7: Preflight check

Add a process-backend preflight check to
`internal/backend/process/preflight.go`. Follows the same pattern as
`config_checks.go` in `internal/preflight`: individual `check*`
functions return a `preflight.Result`, and the top-level
`RunPreflight` function collects them into a `preflight.Report`.

The check functions live in the `process` package (not the
`preflight` package) to keep the cycle broken: `internal/preflight`
becomes a leaf package containing only `Report`, `Result`, severity
constants, log formatting, and shared helpers. Both `backend/docker`
and `backend/process` import it for the types. The same pattern
applies to docker — `internal/preflight/docker_checks.go` moves to
`internal/backend/docker/preflight.go` (see decoupling deliverable).

`ProcessBackend.Preflight()` simply calls the package-local
`RunPreflight` function and returns its report.

```go
package process

import (
    "fmt"
    "os"
    "os/exec"
    "strconv"
    "strings"
    "time"

    "github.com/cynkra/blockyard/internal/config"
    "github.com/cynkra/blockyard/internal/preflight"
)

// RunPreflight verifies the process backend prerequisites. Called by
// (*ProcessBackend).Preflight() with the full config so the egress
// probe can read Redis/OpenBao/database addresses and the resource-
// limit check can read server-level defaults.
//
// Check ordering matters: bwrap/R/userns are prerequisites for
// checkBwrapHostUIDMapping (it spawns bwrap), and that check is a
// prerequisite for checkWorkerEgress (which also spawns bwrap and
// whose results are meaningful only if the host UID mapping is
// effective). If a prerequisite fails we still run the later checks
// — they'll fail too, and emitting all failures at once is more
// useful than bailing at the first.
func RunPreflight(cfg *config.ProcessConfig, fullCfg *config.Config) *preflight.Report {
    r := &preflight.Report{RanAt: time.Now().UTC()}
    r.Add(checkBwrap(cfg))
    r.Add(checkRBinary(cfg))
    r.Add(checkUserNamespaces())
    r.Add(checkPortRange(cfg))
    r.Add(checkResourceLimits(&fullCfg.Server))
    r.Add(checkBwrapHostUIDMapping(cfg))
    r.Add(checkWorkerEgress(cfg, fullCfg))
    return r
}

func checkBwrap(cfg *config.ProcessConfig) preflight.Result {
    if _, err := exec.LookPath(cfg.BwrapPath); err != nil {
        return preflight.Result{
            Name:     "bwrap_available",
            Severity: preflight.SeverityError,
            Message:  fmt.Sprintf("bwrap not found at %q", cfg.BwrapPath),
            Category: "process",
        }
    }
    out, err := exec.Command(cfg.BwrapPath, "--version").CombinedOutput()
    if err != nil {
        return preflight.Result{
            Name:     "bwrap_available",
            Severity: preflight.SeverityError,
            Message:  fmt.Sprintf("bwrap --version failed: %v", err),
            Category: "process",
        }
    }
    return preflight.Result{
        Name:     "bwrap_available",
        Severity: preflight.SeverityOK,
        Message:  fmt.Sprintf("bwrap version: %s", strings.TrimSpace(string(out))),
        Category: "process",
    }
}

func checkRBinary(cfg *config.ProcessConfig) preflight.Result {
    if _, err := exec.LookPath(cfg.RPath); err != nil {
        return preflight.Result{
            Name:     "r_binary",
            Severity: preflight.SeverityError,
            Message:  fmt.Sprintf("R not found at %q", cfg.RPath),
            Category: "process",
        }
    }
    return preflight.Result{
        Name:     "r_binary",
        Severity: preflight.SeverityOK,
        Message:  "R binary found",
        Category: "process",
    }
}

func checkUserNamespaces() preflight.Result {
    data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
    if err != nil {
        // File doesn't exist — kernel allows unprivileged userns by default.
        return preflight.Result{
            Name:     "user_namespaces",
            Severity: preflight.SeverityOK,
            Message:  "unprivileged user namespaces available (sysctl absent, default allow)",
            Category: "process",
        }
    }
    if strings.TrimSpace(string(data)) == "0" {
        return preflight.Result{
            Name:     "user_namespaces",
            Severity: preflight.SeverityError,
            Message:  "unprivileged user namespaces disabled (kernel.unprivileged_userns_clone = 0); required for bwrap --unshare-user",
            Category: "process",
        }
    }
    return preflight.Result{
        Name:     "user_namespaces",
        Severity: preflight.SeverityOK,
        Message:  "unprivileged user namespaces enabled",
        Category: "process",
    }
}

// checkBwrapHostUIDMapping verifies that bwrap's --uid/--gid flags
// produce a host-visible UID/GID, not just a namespace-local one.
// This is load-bearing for decision #5: the operator's iptables
// owner-match rules only fire if workers actually appear as the
// configured worker UID/GID from the init namespace's perspective.
//
// The check works by spawning a bwrap child under a probe UID/GID
// distinct from the caller's UID/GID, then reading the host-side
// /proc/<child_pid>/status from the parent process. If the reported
// Uid/Gid lines do not match what we asked for, bwrap is running in
// unprivileged-userns mode and the mapping is local-only.
//
// Remediation: run blockyard as root (typical containerized mode) or
// install bwrap setuid on the host (`chmod u+s /usr/bin/bwrap`, or
// equivalent via setcap). On Debian 12+/Ubuntu 24.04+ bwrap is no
// longer shipped setuid by default.
func checkBwrapHostUIDMapping(cfg *config.ProcessConfig) preflight.Result {
    const name = "bwrap_host_uid_mapping"

    // Probe UID/GID — must be distinct from any UID the caller might
    // already have. The worker UID range start is a safe choice: it's
    // outside the usual 0/1000 system range and matches the real
    // worker mapping we care about.
    probeUID := cfg.WorkerUIDStart
    probeGID := cfg.WorkerGID
    if probeUID == os.Getuid() {
        // Caller already runs as WorkerUIDStart — pick any other value.
        probeUID = cfg.WorkerUIDStart + 1
    }

    // Long-enough sleep that we have time to read /proc before exit.
    args := []string{
        "--ro-bind", "/", "/",
        "--tmpfs", "/tmp",
        "--proc", "/proc",
        "--dev", "/dev",
        "--unshare-pid", "--unshare-user", "--unshare-uts",
        "--uid", strconv.Itoa(probeUID),
        "--gid", strconv.Itoa(probeGID),
        "--die-with-parent", "--new-session",
        "--cap-drop", "ALL",
        "--", "/bin/sleep", "2",
    }
    cmd := exec.Command(cfg.BwrapPath, args...) //nolint:gosec // G204
    if err := cmd.Start(); err != nil {
        return preflight.Result{
            Name:     name,
            Severity: preflight.SeverityError,
            Message:  fmt.Sprintf("failed to spawn bwrap probe: %v", err),
            Category: "process",
        }
    }
    defer func() {
        _ = cmd.Process.Kill()
        _ = cmd.Wait()
    }()

    // Poll the bwrap child's /proc/<pid>/status — the sandboxed sleep
    // is a grandchild, but what matters for iptables is what the
    // worker processes look like from the host. bwrap and its
    // descendants all share the same host credentials set, so reading
    // the bwrap pid is sufficient.
    var uidLine, gidLine string
    deadline := time.Now().Add(1 * time.Second)
    for time.Now().Before(deadline) {
        data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", cmd.Process.Pid))
        if err == nil {
            for _, line := range strings.Split(string(data), "\n") {
                switch {
                case strings.HasPrefix(line, "Uid:"):
                    uidLine = line
                case strings.HasPrefix(line, "Gid:"):
                    gidLine = line
                }
            }
            if uidLine != "" && gidLine != "" {
                break
            }
        }
        time.Sleep(50 * time.Millisecond)
    }
    if uidLine == "" || gidLine == "" {
        return preflight.Result{
            Name:     name,
            Severity: preflight.SeverityError,
            Message:  "bwrap probe exited before /proc could be read",
            Category: "process",
        }
    }

    // /proc/<pid>/status Uid/Gid lines have the form:
    //   Uid:\t<real>\t<effective>\t<saved>\t<fs>
    // We check the real UID — that's what iptables --uid-owner
    // matches against (filp->f_cred->fsuid == the fsuid, same value
    // on a vanilla fork/exec).
    realHostUID, err := parseStatusUID(uidLine)
    if err != nil {
        return preflight.Result{
            Name:     name,
            Severity: preflight.SeverityError,
            Message:  fmt.Sprintf("could not parse /proc/<pid>/status Uid line %q: %v", uidLine, err),
            Category: "process",
        }
    }
    realHostGID, err := parseStatusUID(gidLine)
    if err != nil {
        return preflight.Result{
            Name:     name,
            Severity: preflight.SeverityError,
            Message:  fmt.Sprintf("could not parse /proc/<pid>/status Gid line %q: %v", gidLine, err),
            Category: "process",
        }
    }

    if realHostUID != probeUID || realHostGID != probeGID {
        return preflight.Result{
            Name:     name,
            Severity: preflight.SeverityError,
            Message: fmt.Sprintf(
                "bwrap --uid/--gid do not affect the host view of the child: "+
                    "requested uid=%d gid=%d, host /proc sees uid=%d gid=%d. "+
                    "The operator's iptables --uid-owner/--gid-owner rules will not match "+
                    "worker traffic in this configuration. Either run blockyard as root "+
                    "(typical containerized deployment) or install bwrap setuid on the host "+
                    "(`sudo chmod u+s %s`). See backends.md for details.",
                probeUID, probeGID, realHostUID, realHostGID, cfg.BwrapPath,
            ),
            Category: "process",
        }
    }
    return preflight.Result{
        Name:     name,
        Severity: preflight.SeverityOK,
        Message:  fmt.Sprintf("bwrap --uid/--gid are host-effective (child host uid=%d gid=%d)", realHostUID, realHostGID),
        Category: "process",
    }
}

// parseStatusUID extracts the first numeric field from a
// /proc/<pid>/status Uid: or Gid: line (the "real" id).
//   Uid:\t1000\t1000\t1000\t1000
func parseStatusUID(line string) (int, error) {
    fields := strings.Fields(line)
    if len(fields) < 2 {
        return 0, fmt.Errorf("too few fields")
    }
    return strconv.Atoi(fields[1])
}

// checkResourceLimits warns when default_memory_limit or default_cpu_limit
// are set but the process backend cannot enforce them (decision #6).
// The fields live in [server] (not [docker]) so the same TOML works
// for Docker and a future k8s backend, but the process backend silently
// ignoring them would be a footgun. The warning makes the gap explicit.
func checkResourceLimits(srvCfg *config.ServerConfig) preflight.Result {
    var unset []string
    if srvCfg.DefaultMemoryLimit != "" {
        unset = append(unset, fmt.Sprintf("default_memory_limit=%q", srvCfg.DefaultMemoryLimit))
    }
    if srvCfg.DefaultCPULimit != 0 {
        unset = append(unset, fmt.Sprintf("default_cpu_limit=%v", srvCfg.DefaultCPULimit))
    }
    if len(unset) == 0 {
        return preflight.Result{
            Name:     "resource_limits",
            Severity: preflight.SeverityOK,
            Message:  "no per-worker resource limits configured",
            Category: "process",
        }
    }
    return preflight.Result{
        Name:     "resource_limits",
        Severity: preflight.SeverityWarning,
        Message: fmt.Sprintf(
            "process backend does not enforce per-worker resource limits; ignoring %s. "+
                "Use the Docker backend if you need cgroup-enforced limits.",
            strings.Join(unset, ", "),
        ),
        Category: "process",
    }
}

func checkPortRange(cfg *config.ProcessConfig) preflight.Result {
    portCount := cfg.PortRangeEnd - cfg.PortRangeStart + 1
    if portCount < 10 {
        return preflight.Result{
            Name:     "port_range",
            Severity: preflight.SeverityWarning,
            Message:  fmt.Sprintf("port range only has %d ports; consider widening [process] port_range_start/port_range_end", portCount),
            Category: "process",
        }
    }
    return preflight.Result{
        Name:     "port_range",
        Severity: preflight.SeverityOK,
        Message:  fmt.Sprintf("port range: %d ports available", portCount),
        Category: "process",
    }
}

// checkWorkerEgress verifies that workers cannot reach sensitive
// network endpoints. It spawns the blockyard binary in `probe` mode
// inside a bwrap sandbox configured exactly like a real worker — same
// UID, same GID, same namespace flags — and asks it to TCP-connect
// to a list of targets. Any successful connection from inside the
// sandbox means a real worker would also succeed, indicating the
// operator's egress firewall is missing or misconfigured.
//
// Targets:
//   - 169.254.169.254:80 (cloud metadata) — always probed; ERROR if
//     reachable since there is no legitimate reason for a worker to
//     read instance credentials.
//   - Redis address (if configured) — WARNING if reachable.
//   - OpenBao address (if configured) — WARNING if reachable.
//   - Database TCP address (if not SQLite) — WARNING if reachable.
//
// The probe binary is the same blockyard binary, invoked with
// `blockyard probe --tcp host:port` (see step 7b). It exits 0 on
// successful TCP connect, 1 on failure. No external tools required.
func checkWorkerEgress(cfg *config.ProcessConfig, fullCfg *config.Config) preflight.Result {
    // Build the target list from config.
    type target struct {
        name     string
        addr     string
        critical bool // true → ERROR if reachable; false → WARNING
    }
    targets := []target{
        {name: "cloud_metadata", addr: "169.254.169.254:80", critical: true},
    }
    if fullCfg.Redis != nil && fullCfg.Redis.URL != "" {
        if hp := tcpAddrFromRedisURL(fullCfg.Redis.URL); hp != "" {
            targets = append(targets, target{name: "redis", addr: hp})
        }
    }
    if fullCfg.Openbao != nil && fullCfg.Openbao.Address != "" {
        if hp := tcpAddrFromHTTPURL(fullCfg.Openbao.Address); hp != "" {
            targets = append(targets, target{name: "openbao", addr: hp})
        }
    }
    if hp := tcpAddrFromDBConfig(fullCfg.Database); hp != "" {
        targets = append(targets, target{name: "database", addr: hp})
    }

    // Use the start of the worker UID range as the probe UID. Preflight
    // runs at startup before any worker spawns, so the allocator state
    // is irrelevant — there's nothing to collide with.
    probeUID := cfg.WorkerUIDStart
    probeGID := cfg.WorkerGID

    var reachable, blocked []string
    var critical bool
    for _, t := range targets {
        if probeReachable(cfg, probeUID, probeGID, t.addr) {
            reachable = append(reachable, fmt.Sprintf("%s (%s)", t.name, t.addr))
            if t.critical {
                critical = true
            }
        } else {
            blocked = append(blocked, t.name)
        }
    }

    if len(reachable) == 0 {
        return preflight.Result{
            Name:     "worker_egress",
            Severity: preflight.SeverityOK,
            Message:  fmt.Sprintf("worker access to internal services is blocked: %s", strings.Join(blocked, ", ")),
            Category: "process",
        }
    }
    severity := preflight.SeverityWarning
    if critical {
        severity = preflight.SeverityError
    }
    return preflight.Result{
        Name:     "worker_egress",
        Severity: severity,
        Message: fmt.Sprintf(
            "workers can reach internal services: %s. "+
                "Install destination-scoped iptables rules, e.g. "+
                "`iptables -A OUTPUT -m owner --gid-owner %d -d <service-ip> -j REJECT` "+
                "for each internal endpoint. Do not use a blanket REJECT — "+
                "workers legitimately need the open internet. "+
                "See backends.md for details.",
            strings.Join(reachable, ", "), cfg.WorkerGID,
        ),
        Category: "process",
    }
}

// probeReachable spawns the blockyard binary in probe mode under the
// same bwrap config a worker would use, and reports whether the
// target TCP address is reachable. Returns false on probe error
// (treated as "not reachable" — fail-safe for the warning, not for
// security).
func probeReachable(cfg *config.ProcessConfig, uid, gid int, target string) bool {
    self, err := os.Executable()
    if err != nil {
        return false
    }
    args := []string{
        "--unshare-pid", "--unshare-user", "--unshare-uts",
        "--uid", strconv.Itoa(uid),
        "--gid", strconv.Itoa(gid),
        "--die-with-parent", "--new-session",
        "--ro-bind", "/", "/",
        "--tmpfs", "/tmp",
        "--proc", "/proc",
        "--dev", "/dev",
        "--chdir", "/tmp",
        "--cap-drop", "ALL",
        "--",
        self, "probe", "--tcp", target, "--timeout", "2s",
    }
    cmd := exec.Command(cfg.BwrapPath, args...) //nolint:gosec // G204
    err = cmd.Run()
    return err == nil // exit 0 = connect succeeded
}
```

Helpers `tcpAddrFromRedisURL`, `tcpAddrFromHTTPURL`, and
`tcpAddrFromDBConfig` parse standard URL/DSN forms and return
`host:port` strings (or `""` if the config is local-only, e.g. SQLite
file paths). They live in `internal/preflight/addrs.go` so both the
docker and process backend preflight code can reuse them.

Wiring into `main.go` is handled by the `be.Preflight(ctx)` call
described in step 6 — no backend-specific branching needed.
`(*ProcessBackend).Preflight()` calls `process.RunPreflight(b.cfg, b.fullCfg)`
and returns the resulting report. Because `process.RunPreflight` lives
in the backend package (not the preflight package), it can freely
import `internal/preflight` for the `Report`/`Result` types without
creating a cycle.

### Step 7b: `blockyard probe` subcommand

The `checkWorkerEgress` preflight needs a binary it can spawn inside
a bwrap sandbox to test TCP reachability. Rather than depending on
external tools (`nc`, `curl`, etc.) that may not be installed in the
sandbox's read-only root, blockyard ships its own probe mode.

In `cmd/blockyard/main.go`, dispatch on the first argument before
`flag.Parse()`:

```go
if len(os.Args) > 1 && os.Args[1] == "probe" {
    if err := runProbe(os.Args[2:]); err != nil {
        os.Exit(1)
    }
    os.Exit(0)
}
```

`runProbe` is a small function (~30 lines) that parses `--tcp host:port`
and `--timeout duration` using a fresh `flag.NewFlagSet("probe",
flag.ContinueOnError)` — **not** the global `flag` package — so its
flag definitions don't clash with main's `-config`/`-version`. It
attempts a TCP connect with the timeout and returns nil on success
or an error on failure. No imports beyond `net`, `flag`, and `time`.

The probe runs inside the bwrap sandbox with the same UID/GID/namespaces
a worker would have. From the firewall's perspective, the connect is
indistinguishable from a worker connect — which is exactly the point.

### Step 8: Orchestrator and rolling update compatibility

The rolling update orchestrator (phase 3-5) uses the Docker API to
clone the server's own container. When the server runs with backend =
"process", the Docker orchestrator is not available — there is no
container to clone.

For phase 3-7, admin endpoints (`/api/v1/admin/update`,
`/api/v1/admin/rollback`, `/api/v1/admin/status`) return `501 Not
Implemented`. The existing guard (`if be, ok :=
srv.Backend.(*docker.DockerBackend)` type assertion in `main.go`)
naturally excludes `ProcessBackend`, leaving `orch = nil`.

**Phase 3-8** adds a process-backend orchestrator variant. The
mechanism is parallel-server cutover, not container cloning: the old
blockyard stays running while a new blockyard starts alongside it on a
different port, an external reverse proxy fronts both, and existing
sessions stay on the old server until they end naturally. The old
server exits when its session count hits zero; its workers die with
it (Pdeathsig), but they have no live sessions left at that point.

Workers are *not* handed off between servers. Each server owns its own
workers for its full lifetime. This is a deliberate design choice: see
the worker survival discussion below. The implication is that the new
server starts with an empty worker pool and the autoscaler rebuilds it
from new traffic — a cold-pool degradation versus the Docker rolling
update (which inherits warm workers via Redis worker state).

**Prerequisites for phase 3-8 process rolling updates:**

- **Redis** — same as Docker rolling updates. Sessions must be in
  Redis so the new server can serve sessions whose cookies were issued
  by the old server.
- **External reverse proxy with service discovery.** Two blockyards on
  the same host bind different ports; an external proxy (Caddy,
  Traefik, nginx with consul-template, etc.) discovers both and routes
  to either. Same operational shape as the Docker rolling update,
  minus the Docker label discovery.
- **Port range sized for overlap.** Both servers allocate worker ports
  from the same `[process] port_range`. During the overlap window the
  range must accommodate the union of both servers' workers. Operators
  who run close to range capacity will need to widen the range before
  enabling rolling updates. See the `[process]` config section.

**Worker survival was considered and rejected.** An alternative design
("option C" in the phase 3-7 review) would drop `Pdeathsig` so workers
survive their parent server, persist worker state to Redis, write
worker logs to files, and add a TTL-based reaper. The new server would
adopt the old server's workers via Redis instead of spawning fresh
ones.

The benefit of that design is narrow: it only matters for users whose
WebSocket is dropped *during* the cutover and reconnects within
seconds. In the parallel-server model the old server keeps running,
those users never see a disconnect, and there is nothing to "preserve"
across a reconnect that does not happen. The two scenarios where
worker survival would help — old server crash mid-drain, or operator
forcing fast cutover with active sessions — are niche failure modes
for the process backend's stated use case (scale-to-zero, internal-
only). The phase 3-7 invasiveness (drop Pdeathsig, persist state, log
files, reaper) is not justified by the realized benefit.

The cost of *not* doing worker survival is the cold-pool degradation
mentioned above. This is acceptable for the use case: scale-to-zero
deployments expect cold starts, and internal-only deployments have
infrequent rolling updates.

### Step 9: Phase 3-9 (pre-fork) forward compatibility

Phase 3-9 adds a pre-fork worker pool with a control channel between
blockyard and each pooled worker (send `RunApp`, receive health,
etc.). The Docker backend uses per-worker bridge IPs on a fixed
control port with token auth. For parity, the process backend needs
an equivalent control transport.

Phase 3-7 does not implement that transport — it belongs to phase 3-9
— but it establishes three load-bearing contracts that phase 3-9
hangs off. Implementers of phase 3-7 should preserve them:

1. **Token dir mount at `/var/run/blockyard`** — set up in `bwrapArgs`
   (step 3). Phase 3-9 reads the per-worker control token from this
   directory to authenticate control connections. The mount path and
   read-only flag are part of the contract.
2. **bwrap does not `--unshare-net`** — workers share the host
   loopback. Phase 3-9 addresses pooled workers via
   `127.0.0.1:<control_port>` without any veth or namespace machinery.
   This is already load-bearing for the `host='127.0.0.1'` Shiny
   binding (decision #20) and the preflight egress probe (step 7);
   phase 3-9 adds a third dependency.
3. **Port allocator is per-worker state, not per-port semantics** —
   phase 3-9 extends `portAllocator` (step 2) to hand out a unique
   control port in addition to the shiny port. The concrete scheme
   (paired allocation, a second allocator over a disjoint range, or
   odd/even within a single range) is deferred to phase 3-9. Phase
   3-7 should avoid baking "one port per worker" into surrounding
   code: `Spawn` calls `ports.Alloc()` once and stores the result in
   `workerProc.port`, which phase 3-9 extends with a second call and
   a second field — no structural changes to the lifecycle flow.

**Sketch (full design in phase-3-9.md).** The process-backend
pre-fork control transport is TCP on localhost with token auth, same
wire protocol as the Docker backend: first frame from the controller
is `AUTH <token>\n`, token read from the mounted
`/var/run/blockyard` directory. The abstract `Forking` interface has
two implementations (Docker, process), both addressing children by
`host:port` — the Docker impl uses the per-worker bridge IP, the
process impl uses `127.0.0.1 + allocated control port`.
`prefork.Manager` is fully backend-agnostic; only the constructor
differs.

**Why TCP, not a Unix socket.** A pathname socket on the host
filesystem would be the obvious alternative and avoid touching
loopback, but it runs straight into the per-worker UID question
(decision #5): the bind-mounted socket file inherits the creator's
UID, and the bwrap worker runs under a different UID than blockyard.
We already dodge this on the Docker side; matching that for the
process backend means staying on TCP. Abstract-namespace sockets
sidestep the filesystem permission issue but are Linux-specific,
harder to debug, and save nothing meaningful over loopback TCP.

### Step 10: Tests

#### Unit tests — bwrap argument construction

`internal/backend/process/bwrap_test.go`:

```go
func TestBwrapArgs(t *testing.T) {
    cfg := &config.ProcessConfig{
        BwrapPath: "/usr/bin/bwrap",
        RPath:     "/usr/bin/R",
    }
    spec := backend.WorkerSpec{
        WorkerID:    "w-1",
        BundlePath:  "/data/bundles/app1/v1",
        WorkerMount: "/app",
        ShinyPort:   3838,
    }

    args := bwrapArgs(cfg, spec, 10000, 60000, 65534)

    // Verify namespace flags are present.
    assertContains(t, args, "--unshare-pid")
    assertContains(t, args, "--unshare-user")
    assertContains(t, args, "--die-with-parent")

    // Verify UID/GID flags are present and correct.
    assertFlagValue(t, args, "--uid", "60000")
    assertFlagValue(t, args, "--gid", "65534")

    // Verify read-only root bind.
    assertBindMount(t, args, "--ro-bind", "/", "/")

    // Verify app bundle is mounted read-only (shadows root).
    assertBindMount(t, args, "--ro-bind", spec.BundlePath, spec.WorkerMount)

    // Verify the R command is after the -- separator.
    sepIdx := indexOf(args, "--")
    if sepIdx < 0 {
        t.Fatal("missing -- separator")
    }
    if args[sepIdx+1] != cfg.RPath {
        t.Errorf("expected R path %q after --, got %q", cfg.RPath, args[sepIdx+1])
    }
}

func TestBwrapArgsWithLibDir(t *testing.T) {
    cfg := &config.ProcessConfig{
        BwrapPath: "/usr/bin/bwrap",
        RPath:     "/usr/bin/R",
    }
    spec := backend.WorkerSpec{
        WorkerID:    "w-1",
        BundlePath:  "/data/bundles/app1/v1",
        LibDir:      "/data/.pkg-store/abc123",
        WorkerMount: "/app",
    }

    args := bwrapArgs(cfg, spec, 10001, 60001, 65534)
    assertBindMount(t, args, "--ro-bind", spec.LibDir, "/blockyard-lib-store")
}

func TestBwrapArgsCustomCmd(t *testing.T) {
    cfg := &config.ProcessConfig{
        BwrapPath: "/usr/bin/bwrap",
        RPath:     "/usr/bin/R",
    }
    spec := backend.WorkerSpec{
        WorkerID:    "w-1",
        BundlePath:  "/data/bundles/app1/v1",
        WorkerMount: "/app",
        Cmd:         []string{"/usr/bin/R", "-e", "httpuv::runServer('0.0.0.0', 8080)"},
    }

    args := bwrapArgs(cfg, spec, 10002, 60002, 65534)
    sepIdx := indexOf(args, "--")
    cmd := args[sepIdx+1:]
    if len(cmd) != 3 || cmd[0] != "/usr/bin/R" {
        t.Errorf("expected custom command after --, got %v", cmd)
    }
}

func TestBwrapBuildArgs(t *testing.T) {
    cfg := &config.ProcessConfig{
        BwrapPath: "/usr/bin/bwrap",
        RPath:     "/usr/bin/R",
    }
    // Mount targets match the real build caller in
    // server/packages.go: `/worker-lib` for the read-only reference
    // library, `/store` for the writable package store destination.
    spec := backend.BuildSpec{
        Cmd: []string{"/usr/bin/R", "-e", "pak::pak_install()"},
        Mounts: []backend.MountEntry{
            {Source: "/data/worker-lib", Target: "/worker-lib", ReadOnly: true},
            {Source: "/data/.pkg-store", Target: "/store", ReadOnly: false},
        },
    }

    args := bwrapBuildArgs(cfg, spec, 60000, 65534)
    assertBindMount(t, args, "--ro-bind", "/data/worker-lib", "/worker-lib")
    assertBindMount(t, args, "--bind", "/data/.pkg-store", "/store")
    assertFlagValue(t, args, "--uid", "60000")
    assertFlagValue(t, args, "--gid", "65534")
}
```

#### Unit tests — port allocator

`internal/backend/process/ports_test.go`:

```go
func TestPortAllocator(t *testing.T) {
    p := newPortAllocator(10000, 10002)

    // Allocate all three ports.
    p1, _ := p.Alloc()
    p2, _ := p.Alloc()
    p3, _ := p.Alloc()

    if p1 != 10000 || p2 != 10001 || p3 != 10002 {
        t.Errorf("expected 10000-10002, got %d, %d, %d", p1, p2, p3)
    }

    // Fourth allocation fails.
    _, err := p.Alloc()
    if err == nil {
        t.Error("expected error when all ports in use")
    }

    // Release and re-allocate.
    p.Release(10001)
    got, err := p.Alloc()
    if err != nil {
        t.Fatal(err)
    }
    if got != 10001 {
        t.Errorf("expected 10001 after release, got %d", got)
    }
}

func TestPortAllocatorConcurrent(t *testing.T) {
    p := newPortAllocator(10000, 10099)
    var wg sync.WaitGroup
    ports := make(chan int, 100)

    for range 100 {
        wg.Add(1)
        go func() {
            defer wg.Done()
            port, err := p.Alloc()
            if err == nil {
                ports <- port
            }
        }()
    }
    wg.Wait()
    close(ports)

    seen := make(map[int]bool)
    for port := range ports {
        if seen[port] {
            t.Errorf("duplicate port allocation: %d", port)
        }
        seen[port] = true
    }
    if len(seen) != 100 {
        t.Errorf("expected 100 unique ports, got %d", len(seen))
    }
}
```

#### Unit tests — log buffer

`internal/backend/process/logs_test.go`:

```go
func TestLogBuffer(t *testing.T) {
    lb := newLogBuffer(100)
    r, w := io.Pipe()

    go lb.ingest(r)
    fmt.Fprintln(w, "line 1")
    fmt.Fprintln(w, "line 2")
    w.Close()

    // Give ingest time to finish.
    time.Sleep(50 * time.Millisecond)

    stream := lb.stream()
    defer stream.Close()

    var lines []string
    for line := range stream.Lines {
        lines = append(lines, line)
        if len(lines) == 2 {
            break
        }
    }
    if lines[0] != "line 1" || lines[1] != "line 2" {
        t.Errorf("unexpected lines: %v", lines)
    }
}

func TestLogBufferRingOverflow(t *testing.T) {
    lb := newLogBuffer(3)
    r, w := io.Pipe()

    go lb.ingest(r)
    for i := range 10 {
        fmt.Fprintf(w, "line %d\n", i)
    }
    w.Close()

    time.Sleep(50 * time.Millisecond)

    stream := lb.stream()
    defer stream.Close()

    var lines []string
    for line := range stream.Lines {
        lines = append(lines, line)
    }
    // Only the last 3 lines should be in the buffer.
    if len(lines) != 3 {
        t.Errorf("expected 3 lines, got %d", len(lines))
    }
}
```

#### Integration tests — worker lifecycle

Two files, both guarded by `//go:build process_test`:

- `internal/backend/process/process_integration_test.go` —
  `package process_test` (external). Exercises the exported API only
  (`process.New`, `Spawn`, `WorkerResourceUsage`, etc.).
- `internal/backend/process/preflight_internal_test.go` —
  `package process` (internal). Holds `TestCheckBwrapHostUIDMapping`
  and any future tests that need to call unexported check functions.

```go
//go:build process_test

package process_test

import (
    "context"
    "os/exec"
    "testing"
    "time"

    "github.com/cynkra/blockyard/internal/backend"
    "github.com/cynkra/blockyard/internal/backend/process"
    "github.com/cynkra/blockyard/internal/config"
)

func TestSpawnAndStop(t *testing.T) {
    if _, err := exec.LookPath("bwrap"); err != nil {
        t.Skip("bwrap not available")
    }

    cfg := &config.Config{
        Process: &config.ProcessConfig{
            BwrapPath:      "bwrap",
            RPath:          "/usr/bin/R",
            PortRangeStart: 19000,
            PortRangeEnd:   19099,
            WorkerUIDStart: 69000,
            WorkerUIDEnd:   69099,
            WorkerGID:      65534,
        },
    }
    be, err := process.New(cfg)
    if err != nil {
        t.Fatal(err)
    }

    ctx := context.Background()
    spec := backend.WorkerSpec{
        WorkerID:    "test-worker-1",
        BundlePath:  t.TempDir(),
        WorkerMount: "/app",
        ShinyPort:   3838,
    }

    if err := be.Spawn(ctx, spec); err != nil {
        t.Fatal(err)
    }

    // Wait for worker to become healthy.
    deadline := time.Now().Add(30 * time.Second)
    for time.Now().Before(deadline) {
        if be.HealthCheck(ctx, spec.WorkerID) {
            break
        }
        time.Sleep(500 * time.Millisecond)
    }

    // Verify Addr returns a valid address.
    addr, err := be.Addr(ctx, spec.WorkerID)
    if err != nil {
        t.Fatal(err)
    }
    if addr == "" {
        t.Error("expected non-empty address")
    }

    // Stop and verify cleanup.
    if err := be.Stop(ctx, spec.WorkerID); err != nil {
        t.Fatal(err)
    }

    // Worker should no longer appear in ListManaged.
    resources, _ := be.ListManaged(ctx)
    for _, r := range resources {
        if r.ID == spec.WorkerID {
            t.Error("worker still in ListManaged after Stop")
        }
    }
}

func TestWorkerResourceUsageUnknownWorker(t *testing.T) {
    cfg := &config.Config{
        Process: &config.ProcessConfig{
            BwrapPath:      "bwrap",
            RPath:          "/usr/bin/R",
            PortRangeStart: 19100,
            PortRangeEnd:   19199,
            WorkerUIDStart: 69100,
            WorkerUIDEnd:   69199,
            WorkerGID:      65534,
        },
    }
    be, err := process.New(cfg)
    if err != nil {
        t.Fatal(err)
    }

    // Unknown worker → nil stats, nil error.
    stats, err := be.WorkerResourceUsage(context.Background(), "nonexistent")
    if err != nil {
        t.Errorf("expected nil error, got %v", err)
    }
    if stats != nil {
        t.Errorf("expected nil stats for unknown worker, got %+v", stats)
    }
}

func TestWorkerResourceUsageLiveWorker(t *testing.T) {
    // Requires a running worker — spawn one, check stats, stop it.
    if _, err := exec.LookPath("bwrap"); err != nil {
        t.Skip("bwrap not available")
    }

    cfg := &config.Config{
        Process: &config.ProcessConfig{
            BwrapPath:      "bwrap",
            RPath:          "/usr/bin/R",
            PortRangeStart: 19100,
            PortRangeEnd:   19199,
            WorkerUIDStart: 69100,
            WorkerUIDEnd:   69199,
            WorkerGID:      65534,
        },
    }
    be, err := process.New(cfg)
    if err != nil {
        t.Fatal(err)
    }

    ctx := context.Background()
    // Use an explicit sleep command so we have a long-lived process to
    // measure. The fallback Cmd in bwrapArgs runs `R -e shiny::runApp(...)`
    // which would die immediately on an empty bundle — tests that spawn
    // from t.TempDir() must supply their own Cmd.
    spec := backend.WorkerSpec{
        WorkerID:    "stats-worker",
        BundlePath:  t.TempDir(),
        WorkerMount: "/app",
        Cmd:         []string{"/bin/sleep", "60"},
    }
    if err := be.Spawn(ctx, spec); err != nil {
        t.Fatal(err)
    }
    defer be.Stop(ctx, spec.WorkerID)

    // Give the sandboxed process a moment to start and allocate RSS.
    time.Sleep(200 * time.Millisecond)

    stats, err := be.WorkerResourceUsage(ctx, spec.WorkerID)
    if err != nil {
        t.Fatalf("expected no error, got %v", err)
    }
    if stats == nil {
        t.Fatal("expected non-nil stats for live worker")
    }
    if stats.MemoryUsageBytes == 0 {
        t.Error("expected non-zero RSS for a live process")
    }
    if stats.MemoryLimitBytes != 0 {
        t.Errorf("expected 0 memory limit (no cgroup), got %d", stats.MemoryLimitBytes)
    }
}

// TestCheckBwrapHostUIDMapping runs under the internal `process` build
// tag (not `process_test`) because it needs to call the unexported
// `checkBwrapHostUIDMapping` directly. An alternative would be to invoke
// `RunPreflight` and scan the report for the "bwrap_host_uid_mapping"
// result by name, but direct invocation produces clearer failure
// messages. This test file therefore lives in `package process`, not
// `package process_test`; the integration tests above that only touch
// exported API can stay in the external test package.
func TestCheckBwrapHostUIDMapping(t *testing.T) {
    if _, err := exec.LookPath("bwrap"); err != nil {
        t.Skip("bwrap not available")
    }

    cfg := &config.ProcessConfig{
        BwrapPath:      "bwrap",
        WorkerUIDStart: 60000,
        WorkerUIDEnd:   60099,
        WorkerGID:      65534,
    }

    result := checkBwrapHostUIDMapping(cfg)

    // This test is strict about the outcome only when we know what
    // mode bwrap is in. On a CI host where bwrap is not setuid and
    // we are not root, an Error result is the correct answer and
    // demonstrates the check is working. On a host with setuid bwrap
    // or when running as root, OK is the correct answer. In either
    // case the check must complete — not error out parsing /proc or
    // hang on the child.
    switch result.Severity {
    case preflight.SeverityOK:
        // Expected when running as root or with setuid bwrap.
    case preflight.SeverityError:
        // Expected with unprivileged bwrap as a non-root caller.
        // Verify the message names both the requested and observed UIDs
        // so operators can act on it.
        if !strings.Contains(result.Message, "requested uid") {
            t.Errorf("error message missing requested uid context: %q", result.Message)
        }
    default:
        t.Errorf("unexpected severity %v: %q", result.Severity, result.Message)
    }
}
```

## Files changed

| File | Action | Summary |
|------|--------|---------|
| `internal/backend/backend.go` | **update** | Rename `ContainerStats` → `WorkerResourceUsage`, `ContainerStatsResult` → `WorkerResourceUsageResult`; add `CleanupOrphanResources()` and `Preflight()` methods |
| `internal/config/config.go` | **update** | Add `ProcessConfig`, `Backend`/`SkipPreflight` on `ServerConfig`; move `DefaultMemoryLimit`/`DefaultCPULimit` from `DockerConfig` → `ServerConfig`, `StoreRetention` from `DockerConfig` → `StorageConfig`; backward-compat parsing for old TOML locations |
| `internal/backend/docker/docker.go` | **update** | Rename `ContainerStats` → `WorkerResourceUsage`; implement `CleanupOrphanResources()` (move `CleanupOrphanMetadataRules` logic) and `Preflight()` (delegates to the moved-in `docker/preflight.go`); read `DefaultMemoryLimit`/`DefaultCPULimit` from `ServerConfig` |
| `internal/units/memory.go` | **create** | `ParseMemoryLimit()` moved from `internal/backend/docker` |
| `internal/backend/process/process.go` | **create** | `ProcessBackend` struct implementing all twelve `Backend` methods (including `UpdateResources`, which returns `ErrNotSupported` — decision #6); `readProcTreeStats`, `collectDescendants`, `readOneProcStats` helpers |
| `internal/backend/process/bwrap.go` | **create** | `bwrapArgs()`, `bwrapBuildArgs()`, `applySeccomp()`, `spliceBeforeSeparator()` |
| `internal/backend/process/ports.go` | **create** | `portAllocator` with `Alloc()`, `Release()`, `InUse()` |
| `internal/backend/process/uids.go` | **create** | `uidAllocator` for per-worker host UIDs (parallel to `portAllocator`) |
| `internal/backend/process/logs.go` | **create** | `logBuffer` with `ingest()` and `stream()` for LogStream delivery |
| `internal/backend/process/preflight.go` | **create** | `RunPreflight()` and `check*` functions — bwrap, R, user namespaces, port range, resource-limit warning, worker egress probe. Lives in the `process` package (not `internal/preflight`) to break the import cycle that adding `Backend.Preflight()` would otherwise create. Imports `internal/preflight` for `Report`/`Result` types only. |
| `internal/backend/docker/preflight.go` | **create** | Moved from `internal/preflight/docker_checks.go`. Same functions, same logic, new home — keeps backend-specific check code in the backend package and lets `internal/preflight` shrink to a leaf package. |
| `internal/preflight/docker_checks.go` | **delete** | Moved to `internal/backend/docker/preflight.go`. |
| `internal/preflight/docker_checks_test.go` | **move** | Moved to `internal/backend/docker/preflight_test.go` alongside the code under test. |
| `internal/preflight/docker_checks_nodep_test.go` | **move** | Moved to `internal/backend/docker/preflight_nodep_test.go` alongside the code under test. |
| `internal/preflight/redis_network_check_test.go` | **move** | Moved to `internal/backend/docker/redis_network_check_test.go` — the helper under test (`checkRedisOnServiceNetwork`) moves with the rest of the docker check code. |
| `internal/preflight/redis_network_check_docker_test.go` | **move** | Moved to `internal/backend/docker/redis_network_check_docker_test.go` alongside the code under test. |
| `internal/preflight/preflight.go` | **update** | Export `Report.add` → `Report.Add` so check functions in the backend packages can append results. The previous private name worked when all check functions lived in the same package; with the refactor they're external callers. |
| `internal/preflight/addrs.go` | **create** | `tcpAddrFromRedisURL`, `tcpAddrFromHTTPURL`, `tcpAddrFromDBConfig` helpers used by the egress probe. Lives in `internal/preflight` so both backends can reuse it. |
| `internal/preflight/config_checks.go` | **update** | `checkNoDefaultMemoryLimit` and `checkNoDefaultCPULimit` read from `cfg.Server.DefaultMemoryLimit`/`DefaultCPULimit` (the new home) instead of `cfg.Docker.*`. Warning messages updated to reference `server.default_memory_limit` / `server.default_cpu_limit`. |
| `cmd/blockyard/main.go` | **update** | Backend selection switch; `probe` subcommand dispatch (early in main); `be.Preflight()` replaces Docker-specific preflight branching; `be.CleanupOrphanResources()` replaces direct Docker import in startup; store retention reads from `cfg.Storage`; rename `DockerPing` → `BackendPing` |
| `internal/ops/ops.go` | **update** | Remove `docker` import; replace `docker.CleanupOrphanMetadataRules()` with `srv.Backend.CleanupOrphanResources()` |
| `internal/api/apps.go` | **update** | Import `internal/units` instead of `internal/backend/docker` for `ParseMemoryLimit`; skip the `failed to update worker resources` warning when `UpdateResources` returns `backend.ErrNotSupported` (so the process backend does not log one noisy line per worker on every app update) |
| `internal/api/runtime.go` | **update** | Rename `ContainerStats` → `WorkerResourceUsage` |
| `internal/ui/sidebar.go` | **update** | Rename `ContainerStats` → `WorkerResourceUsage` |
| `internal/ui/templates/tab_runtime.html` | **update** | Wrap `MemoryLimitBytes` rendering in a conditional so zero (process backend, or unconfigured Docker) renders just the usage |
| `internal/backend/mock/mock.go` | **update** | Rename `ContainerStats` → `WorkerResourceUsage`; add no-op `CleanupOrphanResources()` and `Preflight()` (existing `UpdateResources` is unchanged — the mock backend continues to honor limits for tests that depend on that behavior) |
| `internal/preflight/checker.go` | **update** | Rename `DockerPing` → `BackendPing` in `RuntimeDeps` |
| `internal/backend/process/bwrap_test.go` | **create** | bwrap argument construction tests |
| `internal/backend/process/ports_test.go` | **create** | Port allocator tests (sequential and concurrent) |
| `internal/backend/process/uids_test.go` | **create** | UID allocator tests (sequential and concurrent) |
| `internal/backend/process/logs_test.go` | **create** | Log buffer and LogStream tests |
| `internal/backend/process/process_integration_test.go` | **create** | Integration tests (spawn, health, stop, stats); `//go:build process_test`; `package process_test` (external — exported API only) |
| `internal/backend/process/preflight_internal_test.go` | **create** | Internal tests for unexported preflight helpers (`checkBwrapHostUIDMapping`); `//go:build process_test`; `package process` |

## Design decisions

1. **Port allocator, not dynamic port detection.** Scanning for a free
   port with `:0` and reading back the assigned port is fragile when
   the process (R/Shiny) binds the port itself — there's no reliable
   way to discover which port Shiny actually chose. A pre-allocated port
   passed via the Shiny `port` argument is deterministic. The range is
   configurable so operators can choose ports that don't conflict with
   other services.

2. **Linear scan, not random allocation.** The bitset scan is O(n) in
   the port range size, but the range is small (~1000) and allocation
   is infrequent (once per worker spawn). Random allocation would risk
   fragmentation and make debugging harder — sequential ports are easier
   to correlate in `ps` and `ss` output.

3. **Ring buffer for logs, not unbounded.** Workers can produce
   unbounded output. A 10,000-line ring buffer captures enough for
   debugging without unbounded memory growth. The Docker backend has
   the same bounded behavior (Docker's log driver rotates).

4. **`WorkerResourceUsage` walks the process tree via `/proc` — no cgroups
   needed.** `cmd.Process.Pid` is the bwrap process, not R. Reading
   only bwrap's stats would show trivial usage (bwrap is a ~100KB C
   program). `readProcTreeStats` walks `/proc/{pid}/task/{tid}/children`
   recursively to find all descendants (R, its child processes) and
   sums VmRSS and utime+stime across the tree. These are available for
   any process owned by the current user — no cgroup delegation needed.
   `MemoryLimitBytes` is 0 (no per-worker limit), but usage is real.
   CPU is reported as a percentage matching the Docker backend's
   semantics — each worker caches its previous `(ticks, timestamp)`
   sample and `WorkerResourceUsage` computes `deltaCPU / deltaTime * 100`.
   The first call returns 0% (no previous sample). Returns nil on
   process exit (race between lookup and `/proc` read).

5. **No in-process network isolation; destination-scoped egress
   filtering via UID/GID firewall rules, verified at preflight.**
   Workers share the host network stack — no `--unshare-net`, no veth
   pairs. Building per-worker network namespaces would require either
   rebuilding Docker's veth machinery (substantial complexity,
   contradicts the "no daemon" pitch) or modifying upstream R packages
   to accept passed-in socket FDs. Both were rejected.

   Instead, the process backend gives operators the *hooks* to enforce
   egress isolation outside the process: each worker runs as a unique
   host UID from a configured pool (`worker_uid_range_start..end`),
   and all workers share a single host GID (`worker_gid`). The
   operator installs destination-scoped iptables rules:

   ```sh
   iptables -A OUTPUT -m owner --uid-owner blockyard -j ACCEPT
   iptables -A OUTPUT -m owner --gid-owner $worker_gid -d 169.254.169.254 -j REJECT
   iptables -A OUTPUT -m owner --gid-owner $worker_gid -d <redis-ip>     -j REJECT
   iptables -A OUTPUT -m owner --gid-owner $worker_gid -d <openbao-ip>   -j REJECT
   iptables -A OUTPUT -m owner --gid-owner $worker_gid -d <database-ip>  -j REJECT
   ```

   This lets blockyard reach Redis/database/OpenBao freely, blocks
   workers from reaching those specific internal services, and leaves
   the open internet reachable for workers that need it (downloading
   data, calling external APIs, fetching models). A blanket
   `--gid-owner $worker_gid -j REJECT` would be wrong — it cuts off
   legitimate egress too.

   The `checkWorkerEgress` preflight check actively verifies the rules
   are in place by spawning a probe under the worker UID/GID and
   trying to TCP-connect to the same internal endpoints
   (`checkWorkerEgress` only probes internal targets, never the open
   internet — workers are expected to reach it). Operators see
   explicit warnings (or errors, for the cloud metadata endpoint) when
   the rules are missing.

   **Host UID mapping is load-bearing.** iptables `--uid-owner` and
   `--gid-owner` match on the *host-side* UID/GID of the process that
   created the socket, not on the namespace-local UID inside the
   sandbox. For bwrap's `--uid N --gid G` flags to produce a
   host-visible UID/GID of N/G, either blockyard must run as root
   (typical containerized mode — bwrap inherits root and can set up
   arbitrary uid_map) or bwrap must be setuid on the host (the default
   on Fedora/RHEL, *not* the default on Debian 12+ / Ubuntu 24.04+
   which rely on unprivileged user namespaces). A native non-root
   blockyard deployment with a distro-default bwrap silently fails:
   workers start fine but all run under blockyard's own host UID, so
   the iptables owner-match rules never fire. The
   `checkBwrapHostUIDMapping` preflight check catches this by spawning
   a bwrap child with a distinct sandbox UID and reading the child's
   host-side `/proc/<pid>/status` to verify the requested UID
   actually took effect.

   **Limitations.** This model gives worker-vs-host-services
   isolation but not cross-worker isolation: two workers in the same
   GID can probe each other's loopback Shiny ports. Multi-tenant
   deployments where compromised-worker → compromised-worker attack
   matters should use the Docker backend (per-worker bridge networks).
   Documented in `backends.md`.

6. **No per-worker resource limits.** Same rationale. cgroup delegation
   is difficult inside containers and adds a systemd dependency on
   native hosts. The outer container's cgroup limits serve as a shared
   ceiling in containerized mode. Two consequences flow from this:
   - `UpdateResources` returns `backend.ErrNotSupported` — there is
     nothing to live-update. `api/apps.go` checks for `ErrNotSupported`
     and skips its warning log so app-update requests don't spam one
     warning per worker.
   - `Spawn` emits a one-time `slog.Warn` when `spec.MemoryLimit` or
     `spec.CPULimit` is non-zero. `checkResourceLimits` catches the
     `[server]` defaults at startup, but apps can set their own limits
     via the API after startup, and those land in `WorkerSpec` fields.
     Silently ignoring them would be a footgun; the warning gives
     operators a breadcrumb that the limit did not take effect.

7. **SIGTERM → SIGKILL escalation with 10s grace.** Matches Docker's
   default `docker stop` behavior. `Stop()` sends SIGTERM to the bwrap
   process, which forwards it to the sandboxed R child (bwrap installs
   a signal handler that relays signals to the child). R/Shiny handles
   SIGTERM and shuts down cleanly in most cases. The 10s fallback
   prevents hung processes from blocking worker replacement
   indefinitely.

8. **Two-level death signal for orphan prevention.**
   `--die-with-parent` in bwrap args sets `PR_SET_PDEATHSIG(SIGKILL)`
   on the sandboxed R process — if bwrap dies, R dies. But
   `--die-with-parent` alone does **not** kill bwrap when blockyard
   crashes (bwrap's parent death signal refers to its own child, not
   to its parent). We set `SysProcAttr.Pdeathsig = SIGKILL` on the
   bwrap `exec.Cmd` so the kernel kills bwrap when blockyard exits,
   then `--die-with-parent` cascades to R. `runtime.LockOSThread()`
   around `cmd.Start()` prevents the Go runtime from retiring the
   forking thread, which would trigger a spurious `PDEATHSIG`.

9. **Build streams stdout/stderr line-by-line, not `CombinedOutput`.**
   `spec.LogWriter` is called by the build UI to render progress live
   (pak installs run for minutes); the Docker backend streams via
   `ContainerLogs Follow` + scanner and calls `LogWriter` per line as
   it arrives (see `docker.go` Build step 5). Collecting with
   `CombinedOutput` and replaying lines after the process exits would
   leave the UI stuck on "building..." until completion and then dump
   everything at once — a regression from the Docker backend's UX.
   The process backend uses `StdoutPipe`/`StderrPipe` + two scanner
   goroutines (one per stream to avoid stderr suppression), serialized
   by a mutex so lines are not interleaved mid-character.

10. **`ListManaged` reuses `ResourceContainer` kind.** The process
    backend has no networks to manage — only processes. Reusing
    `ResourceContainer` for processes avoids adding a new `ResourceKind`
    that the rest of the codebase would need to handle. The semantics
    ("a managed thing that can be removed") are the same.

11. **`RemoveResource` uses Kill, not SIGTERM.** `RemoveResource` is
    the orphan cleanup path — called during startup for stale resources.
    There's no need for graceful shutdown of orphaned processes; they
    should die immediately.

12. **Seccomp via fd, not path.** bwrap's `--seccomp` flag takes an
    open file descriptor number, not a file path. The `applySeccomp`
    helper opens the pre-compiled BPF profile, appends it to
    `cmd.ExtraFiles` (which maps to fd 3+), and returns the bwrap args
    referencing that fd. The profile must be BPF binary, not the
    Docker/OCI JSON format — phase 3-8 ships the compiled profile.
    Seccomp is optional at this phase; bwrap's namespace isolation and
    capability dropping are the primary defense.

13. **Preflight check reads `/proc/sys/kernel/unprivileged_userns_clone`.**
    This sysctl controls whether unprivileged processes can create user
    namespaces. When set to `0` (some Debian/Ubuntu defaults), bwrap's
    `--unshare-user` fails. The preflight check catches this at startup
    rather than at first worker spawn, giving operators a clear error
    message.

14. **Two concurrent ingest goroutines, not `io.MultiReader`.** stdout
    and stderr must be consumed concurrently. `io.MultiReader` reads
    sequentially — it waits for the first reader to EOF before starting
    the second, which means stderr is suppressed for the worker's entire
    lifetime. Two goroutines calling `logBuffer.ingest()` independently
    interleave lines in arrival order. The `logBuffer` mutex serializes
    appends. R and Shiny mix diagnostic output across both streams, and
    the Docker backend also merges them — keeping them merged matches
    the `LogStream` contract (single channel).

15. **`exec.Command`, not `exec.CommandContext`.** The `ctx` passed to
    `Spawn` is typically a request context that cancels when the HTTP
    handler returns. `exec.CommandContext` sends SIGKILL on context
    cancellation, which would kill the worker moments after spawning.
    Worker lifecycle is managed explicitly by `Stop()` (SIGTERM →
    SIGKILL) and `SysProcAttr.Pdeathsig` + `--die-with-parent`
    (server crash), not by context propagation.

16. **Minimal environment, not inherited.** Workers get a clean env
    (`PATH`, `HOME=/tmp`, `TMPDIR=/tmp`, `LANG`, `PORT`, plus
    `spec.Env`) instead of inheriting the server's `os.Environ()`. The
    server's environment contains database URLs, Redis credentials,
    OpenBao tokens, and session secrets — passing these to workers
    running arbitrary user code would be a credential leak. The Docker
    backend avoids this by construction (containers start with a clean
    env); the process backend must do it explicitly.

17. **No PID file persistence.** Unlike the Docker backend (which can
    recover container IDs via Docker labels), the process backend tracks
    workers only in memory. If the server crashes, `SysProcAttr.Pdeathsig`
    on the bwrap process (see #8) ensures bwrap dies, and
    `--die-with-parent` cascades to R — no orphans to recover. PID
    files would add complexity for a scenario that shouldn't occur.

18. **`--ro-bind / /`, not path enumeration.** bwrap creates an empty
    mount namespace. Rather than enumerating every system path R needs
    (`/usr`, `/lib`, `/lib64`, `/etc/resolv.conf`, etc.) — which is
    fragile across distros, R versions, and package dependencies — we
    bind the entire host root read-only and selectively mount writable
    paths over it. Isolation comes from read-only access, namespaces,
    seccomp, and capability dropping, not from hiding files. This
    matches Docker's model: workers can read their rootfs, they just
    can't write to it. In containerized mode the root is the outer
    container's minimal filesystem; in native mode more is visible but
    still read-only.

19. **`--chdir /tmp` in bwrap args.** Without `--chdir`, the sandboxed
    process inherits blockyard's working directory. After `--unshare-user`
    remaps the UID, the inherited cwd may not be accessible (e.g., if
    blockyard runs from `/data` owned by root and the sandbox maps to
    nobody). `/tmp` is always available because it's mounted as a fresh
    tmpfs.

20. **`host='127.0.0.1'` for Shiny, not `0.0.0.0`.** The process backend
    does not use `--unshare-net`, so workers share the host network
    stack. Binding to `0.0.0.0` would make the Shiny app directly
    accessible on the host's external interface, bypassing the proxy
    and authentication layer. `127.0.0.1` restricts access to the
    loopback — only the blockyard proxy can reach it.

21. **Backend interface decoupling as a first-class deliverable.** The
    process backend is a stepping stone toward a Kubernetes backend.
    Rather than adding if/else branches for each backend in shared
    code paths, we push backend-specific logic behind the `Backend`
    interface: `Preflight()` for startup checks,
    `CleanupOrphanResources()` for stale resource cleanup. Shared
    config (`default_memory_limit`, `store_retention`) moves out of
    `[docker]` so future backends don't need to read Docker config.
    `ParseMemoryLimit` moves to `internal/units` so API validation
    doesn't import a backend package.

    The goal: after this phase, no code outside `internal/backend/docker/`
    and the composition root (`cmd/blockyard/main.go`) imports that
    package. The composition root is structurally exempt — its job is
    to know about every backend it can construct, including the
    type-asserted orchestrator wiring. Decoupling targets shared code
    paths (`ops`, `api`, `server`, `ui`, `mock`), not the wiring layer.

    Forcing orchestrator construction behind a `Backend.Orchestrator()`
    method was considered and rejected. The Docker, process, and
    future k8s orchestrators have radically different mechanisms
    (container clone + watchdog vs. parallel-process drain vs. k8s
    Deployment rollout); a common interface would either be a
    lowest-common-denominator stub that adds no value or grow to
    accommodate every backend's quirks. Keeping orchestrator wiring in
    the composition root costs three lines per new backend and avoids
    coupling the Backend interface to update concerns.
