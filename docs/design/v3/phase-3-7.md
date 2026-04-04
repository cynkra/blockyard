# Phase 3-7: Process Backend Core

Implement the `Backend` interface using bubblewrap (`bwrap`) for worker
sandboxing — no container runtime, no daemon, no socket. Workers are
spawned as sandboxed child processes with PID/mount/user namespace
isolation, seccomp filtering, and capability dropping. The process
backend targets deployments where startup latency matters (scale-to-zero,
internal-only) or where the Docker socket privilege is unacceptable.

This phase covers the core implementation: config, backend struct,
all nine `Backend` methods, port allocation, bwrap argument
construction, log capture, and tests. Packaging and deployment
artifacts (seccomp profile, Dockerfile, release binaries) are deferred
to phase 3-8.

Independent of the operations track (phases 3-2 through 3-5). Can be
developed in parallel with phase 3-6 (data mounts) and phase 3-9
(pre-fork).

---

## Prerequisites from Earlier Phases

- **Phase 3-1** — migration tooling and conventions. Phase 3-7 does
  not add migrations, but follows the same testing conventions.
- **Backend interface** (`internal/backend/backend.go`) — the nine-method
  `Backend` interface, `WorkerSpec`, `BuildSpec`, `BuildResult`,
  `LogStream`, `ManagedResource`, and `ContainerStatsResult` types are
  stable and unchanged.
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
   `seccomp_profile`, `port_range_start`, `port_range_end`.
2. **Backend selection** (`cmd/blockyard/main.go`) — `[server] backend`
   field gains `"process"` as a valid value. Startup instantiates
   `ProcessBackend` or `DockerBackend` accordingly.
3. **Process backend struct** (`internal/backend/process/process.go`) —
   `ProcessBackend` implementing all nine `Backend` interface methods.
4. **Port allocator** (`internal/backend/process/ports.go`) — allocates
   and releases localhost ports from a configured range.
5. **bwrap command construction** (`internal/backend/process/bwrap.go`)
   — builds the bwrap argument list from a `WorkerSpec`.
6. **Log capture** (`internal/backend/process/logs.go`) — captures
   stdout/stderr from child processes and serves them via `LogStream`.
7. **Build support** — bwrap-sandboxed builds with write access to the
   build output directory. Same `BuildSpec` → R script → pak flow as
   the Docker backend.
8. **Preflight check** — verify `bwrap` is installed and user namespaces
   are enabled (`/proc/sys/kernel/unprivileged_userns_clone`).
9. **Tests** — unit tests for bwrap argument construction, port
   allocation, and log capture. Integration tests (tagged
   `process_test`) for spawn/health/stop lifecycle. Skipped when bwrap
   is unavailable.

## Step-by-step

### Step 1: Config additions

Add `ProcessConfig` to `internal/config/config.go`:

```go
type ProcessConfig struct {
    BwrapPath      string `toml:"bwrap_path"`       // path to bubblewrap binary
    RPath          string `toml:"r_path"`            // path to R binary
    SeccompProfile string `toml:"seccomp_profile"`   // path to custom seccomp JSON; empty = built-in
    PortRangeStart int    `toml:"port_range_start"`  // first port for workers (inclusive)
    PortRangeEnd   int    `toml:"port_range_end"`    // last port for workers (inclusive)
}
```

Add the field to `Config`:

```go
type Config struct {
    // ...existing fields...
    Process *ProcessConfig `toml:"process"` // nil when backend != "process"
}
```

Add `Backend` field to `ServerConfig`:

```go
type ServerConfig struct {
    // ...existing fields...
    Backend string `toml:"backend"` // "docker" (default) or "process"
}
```

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
```

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
func (p *portAllocator) Alloc() (int, error) {
    p.mu.Lock()
    defer p.mu.Unlock()
    for i, taken := range p.used {
        if !taken {
            p.used[i] = true
            return p.start + i, nil
        }
    }
    return 0, fmt.Errorf("process backend: all %d ports in use", len(p.used))
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
func bwrapArgs(cfg *config.ProcessConfig, spec backend.WorkerSpec, port int) []string {
    args := []string{
        // Namespace isolation
        "--unshare-pid",
        "--unshare-user",
        "--unshare-uts",

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

        // App bundle — shadow with the specific bundle path.
        "--ro-bind", spec.BundlePath, spec.WorkerMount,
    }

    // R library (read-only) — legacy path or store-based path
    if spec.LibDir != "" {
        args = append(args, "--ro-bind", spec.LibDir, "/rv-library")
    } else if spec.LibraryPath != "" {
        args = append(args, "--ro-bind", spec.LibraryPath, "/rv-library")
    }

    // Transfer directory (read-write, optional)
    if spec.TransferDir != "" {
        args = append(args, "--bind", spec.TransferDir, "/var/run/blockyard/transfer")
    }

    // Worker token directory (read-only, optional)
    if spec.TokenDir != "" {
        args = append(args, "--ro-bind", spec.TokenDir, "/var/run/blockyard")
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
            fmt.Sprintf("shiny::runApp('%s', port=%s, host='0.0.0.0')",
                spec.WorkerMount, strconv.Itoa(port)),
        )
    }

    return args
}

// bwrapBuildArgs constructs the bwrap arguments for a build task.
// Same root strategy as workers but with additional writable mounts
// for build output.
func bwrapBuildArgs(cfg *config.ProcessConfig, spec backend.BuildSpec) []string {
    args := []string{
        "--unshare-pid",
        "--unshare-user",
        "--unshare-uts",
        "--die-with-parent",
        "--new-session",

        "--ro-bind", "/", "/",
        "--tmpfs", "/tmp",
        "--proc", "/proc",
        "--dev", "/dev",
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
```

The `Spawn` and `Build` methods call `applySeccomp` after creating the
`exec.Cmd` and splice the returned args before the `--` separator. The
file is closed by the OS when the child process execs (Go sets
`O_CLOEXEC` by default, but bwrap reads the fd before exec).

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
// a LogStream. Lines are stored in a bounded ring buffer and
// new subscribers receive the buffered history plus live tail.
type logBuffer struct {
    mu       sync.Mutex
    lines    []string
    maxLines int
    closed   bool
    notify   chan struct{} // signaled on each new line or close
}

func newLogBuffer(maxLines int) *logBuffer {
    return &logBuffer{
        maxLines: maxLines,
        notify:   make(chan struct{}, 1),
    }
}

// ingest reads lines from r until EOF and appends them to the buffer.
func (lb *logBuffer) ingest(r io.Reader) {
    scanner := bufio.NewScanner(r)
    for scanner.Scan() {
        lb.mu.Lock()
        lb.lines = append(lb.lines, scanner.Text())
        if len(lb.lines) > lb.maxLines {
            lb.lines = lb.lines[len(lb.lines)-lb.maxLines:]
        }
        lb.mu.Unlock()
        // Non-blocking signal to subscribers.
        select {
        case lb.notify <- struct{}{}:
        default:
        }
    }
    lb.mu.Lock()
    lb.closed = true
    lb.mu.Unlock()
    select {
    case lb.notify <- struct{}{}:
    default:
    }
}

// stream returns a LogStream that replays buffered lines and follows.
func (lb *logBuffer) stream() backend.LogStream {
    ch := make(chan string, 64)
    done := make(chan struct{})

    go func() {
        defer close(ch)
        cursor := 0
        for {
            lb.mu.Lock()
            snapshot := lb.lines
            closed := lb.closed
            lb.mu.Unlock()

            for cursor < len(snapshot) {
                select {
                case ch <- snapshot[cursor]:
                    cursor++
                case <-done:
                    return
                }
            }
            if closed {
                return
            }
            // Wait for new data.
            select {
            case <-lb.notify:
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
    "context"
    "fmt"
    "log/slog"
    "net"
    "os"
    "os/exec"
    "strconv"
    "strings"
    "sync"
    "syscall"
    "time"

    "github.com/cynkra/blockyard/internal/backend"
    "github.com/cynkra/blockyard/internal/config"
)

// Compile-time interface check.
var _ backend.Backend = (*ProcessBackend)(nil)

// workerProc holds per-worker state.
type workerProc struct {
    cmd     *exec.Cmd
    process *os.Process
    port    int
    spec    backend.WorkerSpec
    logs    *logBuffer
    done    chan struct{} // closed when process exits
}

// ProcessBackend implements backend.Backend using bubblewrap.
type ProcessBackend struct {
    cfg   *config.ProcessConfig
    ports *portAllocator

    mu      sync.Mutex
    workers map[string]*workerProc // keyed by worker ID
}

// New creates a ProcessBackend. Verifies that bwrap exists at the
// configured path.
func New(cfg *config.ProcessConfig) (*ProcessBackend, error) {
    if _, err := exec.LookPath(cfg.BwrapPath); err != nil {
        return nil, fmt.Errorf("process backend: bwrap not found at %q: %w",
            cfg.BwrapPath, err)
    }
    return &ProcessBackend{
        cfg:     cfg,
        ports:   newPortAllocator(cfg.PortRangeStart, cfg.PortRangeEnd),
        workers: make(map[string]*workerProc),
    }, nil
}

func (b *ProcessBackend) Spawn(ctx context.Context, spec backend.WorkerSpec) error {
    port, err := b.ports.Alloc()
    if err != nil {
        return err
    }

    args := bwrapArgs(b.cfg, spec, port)

    // exec.Command, not exec.CommandContext — the ctx passed to Spawn is
    // typically a request context that cancels when the handler returns.
    // CommandContext would SIGKILL the worker on cancellation. Worker
    // lifecycle is managed by Stop() and --die-with-parent, not by ctx.
    cmd := exec.Command(b.cfg.BwrapPath, args...) //nolint:gosec // G204: args from validated config

    // Minimal environment — do NOT inherit the server's env, which
    // contains database URLs, Redis credentials, OpenBao tokens, etc.
    cmd.Env = []string{
        "PATH=/usr/bin:/usr/local/bin:/bin",
        "HOME=/tmp",
        "TMPDIR=/tmp",
        "LANG=C.UTF-8",
    }
    for k, v := range spec.Env {
        cmd.Env = append(cmd.Env, k+"="+v)
    }
    cmd.Env = append(cmd.Env, fmt.Sprintf("PORT=%d", port))

    // Log capture
    logs := newLogBuffer(10000)

    stdout, err := cmd.StdoutPipe()
    if err != nil {
        b.ports.Release(port)
        return fmt.Errorf("process backend: stdout pipe: %w", err)
    }
    stderr, err := cmd.StderrPipe()
    if err != nil {
        b.ports.Release(port)
        return fmt.Errorf("process backend: stderr pipe: %w", err)
    }

    if err := cmd.Start(); err != nil {
        b.ports.Release(port)
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
        slog.Info("process backend: worker exited",
            "worker_id", spec.WorkerID, "port", port)
    }()

    b.mu.Lock()
    b.workers[spec.WorkerID] = &workerProc{
        cmd:     cmd,
        process: cmd.Process,
        port:    port,
        spec:    spec,
        logs:    logs,
        done:    done,
    }
    b.mu.Unlock()

    slog.Info("process backend: spawned worker",
        "worker_id", spec.WorkerID,
        "port", port,
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
    args := bwrapBuildArgs(b.cfg, spec)
    // Context is appropriate here — builds are bounded, run-to-completion
    // tasks. If the caller cancels, the build should stop.
    cmd := exec.CommandContext(ctx, b.cfg.BwrapPath, args...) //nolint:gosec // G204: args from validated config

    // Minimal env + build-specific vars from spec.Env ([]string KEY=VALUE).
    cmd.Env = []string{
        "PATH=/usr/bin:/usr/local/bin:/bin",
        "HOME=/tmp",
        "TMPDIR=/tmp",
        "LANG=C.UTF-8",
    }
    cmd.Env = append(cmd.Env, spec.Env...)

    output, err := cmd.CombinedOutput()
    logs := string(output)

    // Stream log lines to the callback if provided.
    if spec.LogWriter != nil {
        for _, line := range splitLines(logs) {
            spec.LogWriter(line)
        }
    }

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

func (b *ProcessBackend) ContainerStats(_ context.Context, id string) (*backend.ContainerStatsResult, error) {
    b.mu.Lock()
    w, ok := b.workers[id]
    b.mu.Unlock()
    if !ok {
        return nil, nil
    }
    return readProcStats(w.process.Pid)
}

// readProcStats reads RSS and CPU usage from /proc/{pid}/stat and
// /proc/{pid}/status. No cgroup access needed — these are available
// for any process owned by the current user.
func readProcStats(pid int) (*backend.ContainerStatsResult, error) {
    // RSS from /proc/{pid}/status (VmRSS line, in kB).
    statusPath := fmt.Sprintf("/proc/%d/status", pid)
    statusData, err := os.ReadFile(statusPath)
    if err != nil {
        return nil, nil // process exited between lookup and read
    }

    var rssKB uint64
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
    statPath := fmt.Sprintf("/proc/%d/stat", pid)
    statData, err := os.ReadFile(statPath)
    if err != nil {
        return nil, nil
    }

    // /proc/pid/stat format: pid (comm) state ... field14=utime field15=stime
    // Find the closing ')' of comm to avoid issues with spaces in comm.
    statStr := string(statData)
    commEnd := strings.LastIndex(statStr, ")")
    if commEnd < 0 || commEnd+2 >= len(statStr) {
        return nil, nil
    }
    fields := strings.Fields(statStr[commEnd+2:])
    // fields[0]=state, [1..]=field4+. utime=field14 → index 11, stime=field15 → index 12.
    var cpuPercent float64
    if len(fields) > 12 {
        utime, _ := strconv.ParseUint(fields[11], 10, 64)
        stime, _ := strconv.ParseUint(fields[12], 10, 64)
        // Total CPU ticks. To get a percentage we'd need a delta over
        // time, but for a point-in-time snapshot we report cumulative
        // seconds — the caller can diff successive calls.
        clockTick := uint64(100) // sysconf(_SC_CLK_TCK), 100 on Linux
        totalSec := float64(utime+stime) / float64(clockTick)
        cpuPercent = totalSec // cumulative CPU seconds, not a percentage
    }

    return &backend.ContainerStatsResult{
        CPUPercent:       cpuPercent,
        MemoryUsageBytes: rssKB * 1024,
        MemoryLimitBytes: 0, // no per-worker cgroup limit
    }, nil
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
    be, err = process.New(cfg.Process)
    if err != nil {
        slog.Error("failed to create process backend", "error", err)
        os.Exit(1)
    }
default: // "docker"
    be, err = docker.New(context.Background(), &cfg.Docker, cfg.Storage.BundleServerPath)
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

The `docker.Image` validation in `validate()` moves into the `case
"docker"` branch (step 1) so it does not reject process-backend configs
that leave `docker.image` empty.

### Step 7: Preflight check

Add a process-backend preflight check to
`internal/preflight/process_checks.go`. Follows the same pattern as
`config_checks.go`: individual `check*` functions return a `Result`,
and the top-level `Run*Checks` function collects them.

```go
package preflight

import (
    "fmt"
    "os"
    "os/exec"
    "strings"
    "time"

    "github.com/cynkra/blockyard/internal/config"
)

// RunProcessChecks verifies the process backend prerequisites.
func RunProcessChecks(cfg *config.ProcessConfig) *Report {
    r := &Report{RanAt: time.Now().UTC()}
    r.add(checkBwrap(cfg))
    r.add(checkRBinary(cfg))
    r.add(checkUserNamespaces())
    r.add(checkPortRange(cfg))
    return r
}

func checkBwrap(cfg *config.ProcessConfig) Result {
    if _, err := exec.LookPath(cfg.BwrapPath); err != nil {
        return Result{
            Name:     "bwrap_available",
            Severity: SeverityError,
            Message:  fmt.Sprintf("bwrap not found at %q", cfg.BwrapPath),
            Category: "process",
        }
    }
    out, err := exec.Command(cfg.BwrapPath, "--version").CombinedOutput()
    if err != nil {
        return Result{
            Name:     "bwrap_available",
            Severity: SeverityError,
            Message:  fmt.Sprintf("bwrap --version failed: %v", err),
            Category: "process",
        }
    }
    return Result{
        Name:     "bwrap_available",
        Severity: SeverityOK,
        Message:  fmt.Sprintf("bwrap version: %s", strings.TrimSpace(string(out))),
        Category: "process",
    }
}

func checkRBinary(cfg *config.ProcessConfig) Result {
    if _, err := exec.LookPath(cfg.RPath); err != nil {
        return Result{
            Name:     "r_binary",
            Severity: SeverityError,
            Message:  fmt.Sprintf("R not found at %q", cfg.RPath),
            Category: "process",
        }
    }
    return Result{
        Name:     "r_binary",
        Severity: SeverityOK,
        Message:  "R binary found",
        Category: "process",
    }
}

func checkUserNamespaces() Result {
    data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
    if err != nil {
        // File doesn't exist — kernel allows unprivileged userns by default.
        return Result{
            Name:     "user_namespaces",
            Severity: SeverityOK,
            Message:  "unprivileged user namespaces available (sysctl absent, default allow)",
            Category: "process",
        }
    }
    if strings.TrimSpace(string(data)) == "0" {
        return Result{
            Name:     "user_namespaces",
            Severity: SeverityError,
            Message:  "unprivileged user namespaces disabled (kernel.unprivileged_userns_clone = 0); required for bwrap --unshare-user",
            Category: "process",
        }
    }
    return Result{
        Name:     "user_namespaces",
        Severity: SeverityOK,
        Message:  "unprivileged user namespaces enabled",
        Category: "process",
    }
}

func checkPortRange(cfg *config.ProcessConfig) Result {
    portCount := cfg.PortRangeEnd - cfg.PortRangeStart + 1
    if portCount < 10 {
        return Result{
            Name:     "port_range",
            Severity: SeverityWarning,
            Message:  fmt.Sprintf("port range only has %d ports; consider widening [process] port_range_start/port_range_end", portCount),
            Category: "process",
        }
    }
    return Result{
        Name:     "port_range",
        Severity: SeverityOK,
        Message:  fmt.Sprintf("port range: %d ports available", portCount),
        Category: "process",
    }
}
```

Wire it into `main.go` alongside the existing Docker preflight:

```go
if cfg.Server.Backend == "process" {
    processReport := preflight.RunProcessChecks(cfg.Process)
    processReport.Log()
    if processReport.HasErrors() {
        slog.Error("preflight process checks failed")
        os.Exit(1)
    }
}
```

### Step 8: Orchestrator and rolling update compatibility

The rolling update orchestrator (phase 3-5) uses the Docker API to
clone the server's own container. When the server runs with backend =
"process", the orchestrator is not available — there is no container to
clone.

The admin endpoints (`/api/v1/admin/update`, `/api/v1/admin/rollback`,
`/api/v1/admin/status`) already return `501 Not Implemented` when the
server is not running as a Docker container (native mode). The same
applies when `backend = "process"` — the orchestrator is not
instantiated and admin endpoints return 501.

No code change needed — the existing guard (`if be, ok :=
srv.Backend.(*docker.DockerBackend)` type assertion in `main.go`)
naturally excludes `ProcessBackend`.

### Step 9: Tests

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

    args := bwrapArgs(cfg, spec, 10000)

    // Verify namespace flags are present.
    assertContains(t, args, "--unshare-pid")
    assertContains(t, args, "--unshare-user")
    assertContains(t, args, "--die-with-parent")

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

    args := bwrapArgs(cfg, spec, 10001)
    assertBindMount(t, args, "--ro-bind", spec.LibDir, "/rv-library")
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

    args := bwrapArgs(cfg, spec, 10002)
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
    spec := backend.BuildSpec{
        Cmd: []string{"/usr/bin/R", "-e", "pak::pak_install()"},
        Mounts: []backend.MountEntry{
            {Source: "/data/bundles/app1/v1", Target: "/app", ReadOnly: true},
            {Source: "/data/lib-out", Target: "/rv-library", ReadOnly: false},
        },
    }

    args := bwrapBuildArgs(cfg, spec)
    assertBindMount(t, args, "--ro-bind", "/data/bundles/app1/v1", "/app")
    assertBindMount(t, args, "--bind", "/data/lib-out", "/rv-library")
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

`internal/backend/process/process_integration_test.go`, guarded by
`//go:build process_test`:

```go
//go:build process_test

package process_test

import (
    "context"
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

    cfg := &config.ProcessConfig{
        BwrapPath:      "bwrap",
        RPath:          "/usr/bin/R",
        PortRangeStart: 19000,
        PortRangeEnd:   19099,
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

func TestContainerStatsUnknownWorker(t *testing.T) {
    cfg := &config.ProcessConfig{
        BwrapPath:      "bwrap",
        RPath:          "/usr/bin/R",
        PortRangeStart: 19100,
        PortRangeEnd:   19199,
    }
    be, err := process.New(cfg)
    if err != nil {
        t.Fatal(err)
    }

    // Unknown worker → nil stats, nil error.
    stats, err := be.ContainerStats(context.Background(), "nonexistent")
    if err != nil {
        t.Errorf("expected nil error, got %v", err)
    }
    if stats != nil {
        t.Errorf("expected nil stats for unknown worker, got %+v", stats)
    }
}

func TestContainerStatsLiveWorker(t *testing.T) {
    // Requires a running worker — spawn one, check stats, stop it.
    if _, err := exec.LookPath("bwrap"); err != nil {
        t.Skip("bwrap not available")
    }

    cfg := &config.ProcessConfig{
        BwrapPath:      "bwrap",
        RPath:          "/usr/bin/R",
        PortRangeStart: 19100,
        PortRangeEnd:   19199,
    }
    be, err := process.New(cfg)
    if err != nil {
        t.Fatal(err)
    }

    ctx := context.Background()
    spec := backend.WorkerSpec{
        WorkerID:    "stats-worker",
        BundlePath:  t.TempDir(),
        WorkerMount: "/app",
    }
    if err := be.Spawn(ctx, spec); err != nil {
        t.Fatal(err)
    }
    defer be.Stop(ctx, spec.WorkerID)

    stats, err := be.ContainerStats(ctx, spec.WorkerID)
    if err != nil {
        t.Fatalf("expected no error, got %v", err)
    }
    if stats == nil {
        t.Fatal("expected non-nil stats for live worker")
    }
    if stats.MemoryUsageBytes == 0 {
        t.Error("expected non-zero RSS for a live R process")
    }
    if stats.MemoryLimitBytes != 0 {
        t.Errorf("expected 0 memory limit (no cgroup), got %d", stats.MemoryLimitBytes)
    }
}
```

## Files changed

| File | Action | Summary |
|------|--------|---------|
| `internal/config/config.go` | **update** | Add `ProcessConfig` struct, `Backend` field on `ServerConfig`, process defaults, validation |
| `internal/backend/process/process.go` | **create** | `ProcessBackend` struct, `New()`, `Spawn`, `Stop`, `HealthCheck`, `Logs`, `Addr`, `Build`, `ListManaged`, `RemoveResource`, `ContainerStats` |
| `internal/backend/process/bwrap.go` | **create** | `bwrapArgs()` and `bwrapBuildArgs()` — construct bwrap command lines |
| `internal/backend/process/ports.go` | **create** | `portAllocator` with `Alloc()`, `Release()`, `InUse()` |
| `internal/backend/process/logs.go` | **create** | `logBuffer` with `ingest()` and `stream()` for LogStream delivery |
| `cmd/blockyard/main.go` | **update** | Backend selection switch; add `process` import; skip Docker preflight for process backend |
| `internal/preflight/process_checks.go` | **create** | `RunProcessChecks()` — check bwrap, R, user namespaces, port range |
| `internal/backend/process/bwrap_test.go` | **create** | bwrap argument construction tests |
| `internal/backend/process/ports_test.go` | **create** | Port allocator tests (sequential and concurrent) |
| `internal/backend/process/logs_test.go` | **create** | Log buffer and LogStream tests |
| `internal/backend/process/process_integration_test.go` | **create** | Integration tests (spawn, health, stop, stats); `//go:build process_test` |

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

4. **`ContainerStats` reads `/proc/{pid}` — no cgroups needed.** RSS
   comes from `/proc/{pid}/status` (VmRSS), CPU from
   `/proc/{pid}/stat` (utime + stime). These are available for any
   process owned by the current user — no cgroup delegation needed.
   `MemoryLimitBytes` is 0 (no per-worker limit), but usage is real.
   CPU is reported as cumulative seconds rather than a percentage; the
   caller diffs successive calls. Returns nil on process exit (race
   between lookup and `/proc` read).

5. **No network isolation.** Documented as a deliberate scope decision
   in `backends.md`. Workers share the host network stack. Network
   namespaces with veth pairs would add significant complexity for a
   backend whose purpose is simplicity. Deployments needing network
   isolation should use the Docker backend.

6. **No per-worker resource limits.** Same rationale. cgroup delegation
   is difficult inside containers and adds a systemd dependency on
   native hosts. The outer container's cgroup limits serve as a shared
   ceiling in containerized mode.

7. **SIGTERM → SIGKILL escalation with 10s grace.** Matches Docker's
   default `docker stop` behavior. R/Shiny handles SIGTERM and shuts
   down cleanly in most cases. The 10s fallback prevents hung processes
   from blocking worker replacement indefinitely.

8. **`--die-with-parent` in bwrap args.** If the blockyard server
   crashes, all bwrap children die immediately. No orphan processes.
   This is critical — without it, a server restart would find port
   conflicts from the previous run's still-alive workers.

9. **Build uses `CombinedOutput`, not pipes.** Build tasks run to
   completion (not long-lived), so capturing all output into a string
   is simpler and matches the `BuildResult.Logs` return type. The
   Docker backend does the same — build logs are collected after the
   container exits.

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
    SIGKILL) and `--die-with-parent` (server crash), not by context
    propagation.

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
    workers only in memory. If the server crashes, `--die-with-parent`
    ensures workers die too, so there are no orphans to recover. PID
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
