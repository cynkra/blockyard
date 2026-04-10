package process

import (
	"bufio"
	"context"
	"errors"
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
	"github.com/cynkra/blockyard/internal/redisstate"
)

// Compile-time interface check.
var _ backend.Backend = (*ProcessBackend)(nil)

// cpuSample stores a previous CPU reading for delta computation.
type cpuSample struct {
	ticks uint64 // utime + stime in clock ticks
	when  time.Time
}

// workerProc holds per-worker state.
type workerProc struct {
	cmd     *exec.Cmd
	process *os.Process
	port    int
	uid     int // host UID this worker runs as (returned to pool on exit)
	spec    backend.WorkerSpec
	logs    *logBuffer
	done    chan struct{} // closed when process exits
	lastCPU *cpuSample    // previous CPU sample for delta; nil on first call
}

// ProcessBackend implements backend.Backend using bubblewrap.
type ProcessBackend struct {
	cfg     *config.ProcessConfig // shortcut for fullCfg.Process; used in hot paths
	fullCfg *config.Config        // held for Preflight() — needs Redis/OpenBao/DB addrs and Server.DefaultMemoryLimit/CPULimit
	ports   portAllocator
	uids    uidAllocator

	mu      sync.Mutex
	workers map[string]*workerProc // keyed by worker ID
}

// New creates a ProcessBackend. Verifies that bwrap exists at the
// configured path and that the worker mount point can be reached
// from inside the bwrap sandbox. The full config is stored so
// Preflight() can read the addresses of Redis/OpenBao/database for
// the egress probe and the server-level resource-limit fields for
// the warning check.
//
// When rc is non-nil the backend uses Redis-backed port and UID
// allocators to coordinate with concurrent peers during rolling-
// update overlap. When rc is nil (single-node deployment without
// [redis]) it falls back to in-memory bitset allocators, which are
// correct because without the cutover window there are no cross-
// server collisions.
func New(fullCfg *config.Config, rc *redisstate.Client) (*ProcessBackend, error) {
	cfg := fullCfg.Process
	if cfg == nil {
		return nil, fmt.Errorf("process backend: [process] config section is required")
	}
	if _, err := exec.LookPath(cfg.BwrapPath); err != nil {
		return nil, fmt.Errorf("process backend: bwrap not found at %q: %w",
			cfg.BwrapPath, err)
	}
	if err := ensureBundleMountPoint(fullCfg.Storage.BundleWorkerPath); err != nil {
		return nil, err
	}

	var (
		ports portAllocator
		uids  uidAllocator
	)
	if rc != nil {
		hostname, _ := os.Hostname()
		ports = newRedisPortAllocator(rc, cfg.PortRangeStart, cfg.PortRangeEnd, hostname)
		uids = newRedisUIDAllocator(rc, cfg.WorkerUIDStart, cfg.WorkerUIDEnd, hostname)
	} else {
		ports = newMemoryPortAllocator(cfg.PortRangeStart, cfg.PortRangeEnd)
		uids = newMemoryUIDAllocator(cfg.WorkerUIDStart, cfg.WorkerUIDEnd)
	}

	return &ProcessBackend{
		cfg:     cfg,
		fullCfg: fullCfg,
		ports:   ports,
		uids:    uids,
		workers: make(map[string]*workerProc),
	}, nil
}

// ensureBundleMountPoint guarantees that bwrap will be able to bind
// the worker bundle at the configured path. There are two writable
// surfaces in the sandbox bwrap can mkdir under:
//
//  1. The fresh tmpfs we mount at /tmp inside every sandbox. Anything
//     under /tmp works without touching the host filesystem because
//     bwrap creates the directory inside the in-sandbox tmpfs.
//
//  2. The host filesystem itself, brought in via --ro-bind / /. If
//     the configured path exists on the host before bwrap launches,
//     it appears as an empty read-only directory inside the sandbox
//     and bwrap can bind onto it without needing to mkdir.
//
// Anything else fails at the first Spawn with bwrap's confusing
// "Can't mkdir <path>: Read-only file system" error. We catch the
// missing directory at startup and either create it or surface a
// clear error pointing operators at the actionable fixes.
func ensureBundleMountPoint(path string) error {
	if path == "" {
		return fmt.Errorf("process backend: storage.bundle_worker_path must not be empty")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("process backend: storage.bundle_worker_path must be absolute, got %q", path)
	}
	// Strategy 1: under /tmp — handled by the in-sandbox tmpfs.
	if path == "/tmp" || strings.HasPrefix(path, "/tmp/") {
		return nil
	}
	// Strategy 2: the path must exist on the host. Try to create it
	// idempotently; if it already exists we're done.
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("process backend: bundle_worker_path %q exists but is not a directory", path)
		}
		return nil
	}
	if err := os.MkdirAll(path, 0o755); err != nil { //nolint:gosec // G301: directory needs to be readable by every worker UID
		return fmt.Errorf(
			"process backend: cannot create bundle mount point %q on the host: %w. "+
				"The process backend bind-mounts each worker's bundle at this path "+
				"inside the bwrap sandbox; bwrap cannot create the directory itself "+
				"because the sandbox root is read-only. Options:\n"+
				"  • Set storage.bundle_worker_path to a path under /tmp (e.g. /tmp/app) — "+
				"bwrap mounts a fresh tmpfs there inside every sandbox\n"+
				"  • Pre-create %q on the host with mode 0755\n"+
				"  • Run blockyard with permissions to create %q (containerized mode as root)",
			path, err, path, path)
	}
	return nil
}

// Preflight implements backend.Backend by delegating to RunPreflight.
func (b *ProcessBackend) Preflight(_ context.Context) (*preflight.Report, error) {
	return RunPreflight(b.cfg, b.fullCfg), nil
}

// CleanupOrphanResources implements backend.Backend. Workers from a
// previous run are already dead (Pdeathsig killed them with the
// server), so the in-memory variants have nothing to clean up. The
// Redis variants, however, carry owned claims across server restarts
// and need a scan-and-delete at startup to return crashed-session
// ports and UIDs to the pool.
func (b *ProcessBackend) CleanupOrphanResources(ctx context.Context) error {
	type orphanCleaner interface {
		CleanupOwnedOrphans(ctx context.Context) error
	}
	if c, ok := b.uids.(orphanCleaner); ok {
		if err := c.CleanupOwnedOrphans(ctx); err != nil {
			return fmt.Errorf("uid cleanup: %w", err)
		}
	}
	if c, ok := b.ports.(orphanCleaner); ok {
		if err := c.CleanupOwnedOrphans(ctx); err != nil {
			return fmt.Errorf("port cleanup: %w", err)
		}
	}
	return nil
}

func (b *ProcessBackend) Spawn(_ context.Context, spec backend.WorkerSpec) error {
	// Per-app resource limits are silently ignored on this backend
	// (decision #6). The warning lives in api/apps.go at the moment
	// the value is set — emitting it here would fire on every spawn
	// for every app for the lifetime of the deployment.

	// Resolve R version from the bundle manifest. When a specific
	// version is requested and installed via rig, the worker runs
	// against that binary. Otherwise fall back to the configured
	// default (cfg.RPath / the rig shim).
	if spec.RVersion != "" && len(spec.Cmd) > 0 && spec.Cmd[0] == "R" {
		rPath, fell := ResolveRBinary(spec.RVersion, b.cfg.RPath)
		if fell {
			slog.Warn("requested R version not installed, using default",
				"worker_id", spec.WorkerID, "requested", spec.RVersion,
				"fallback", rPath)
		} else {
			slog.Info("resolved R version",
				"worker_id", spec.WorkerID, "version", spec.RVersion,
				"path", rPath)
		}
		cmd := make([]string, len(spec.Cmd))
		copy(cmd, spec.Cmd)
		cmd[0] = rPath
		spec.Cmd = cmd
	}

	// If an entry for this worker ID already exists, refuse to spawn
	// over a live one. An entry that has already exited (its done
	// channel is closed and slots have been released) is cleared so
	// the new spawn can take its place — this matches the Docker
	// semantic where you can recreate a container with the same name
	// after the previous one exited and was removed.
	b.mu.Lock()
	if existing, ok := b.workers[spec.WorkerID]; ok {
		select {
		case <-existing.done:
			delete(b.workers, spec.WorkerID)
		default:
			b.mu.Unlock()
			return fmt.Errorf("process backend: worker %q is already running", spec.WorkerID)
		}
	}
	b.mu.Unlock()

	// Reserve a port AND hold a listener on it. The listener is closed
	// inside the fork goroutine immediately before cmd.Start() so the
	// kernel hands the port directly to the R child with the smallest
	// possible window for another host process to race in. The deferred
	// close is a safety net for error paths that return before the
	// goroutine reaches its explicit close — closing an already-closed
	// listener returns an error we ignore.
	port, ln, err := b.ports.Reserve()
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
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

	releaseSlots := func() {
		b.ports.Release(port)
		b.uids.Release(uid)
	}

	// Seccomp — splice --seccomp <fd> args before the "--" separator.
	// No-op when SeccompProfile is empty (phase 3-8 ships the profile).
	// secCleanup releases the parent-side fd; deferred so it runs on
	// every error path including a Start failure inside the goroutine.
	secArgs, secCleanup, err := applySeccomp(cmd, b.cfg.SeccompProfile)
	if err != nil {
		releaseSlots()
		return fmt.Errorf("process backend: seccomp: %w", err)
	}
	defer secCleanup()
	if len(secArgs) > 0 {
		spliceBeforeSeparator(cmd, secArgs)
	}

	// Minimal environment — do NOT inherit the server's env, which
	// contains database URLs, Redis credentials, OpenBao tokens, etc.
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
	// Process backend shares the host network (no --unshare-net), so
	// workers must bind to loopback to avoid exposing the Shiny port
	// on all interfaces, which would let clients bypass the proxy.
	// Docker workers don't set this, defaulting to 0.0.0.0, which is
	// fine because each container has its own network namespace.
	cmd.Env = append(cmd.Env, "SHINY_HOST=127.0.0.1")

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

	// Fork + wait must run on the same pinned OS thread. Pdeathsig
	// fires when the thread that *forked* the child exits, not when
	// the goroutine hosting that thread is rescheduled. If we locked
	// the thread only around Start() and then unlocked, the Go runtime
	// could retire the thread while the child is still running and
	// SIGKILL bwrap. Instead, dedicate a single goroutine to the
	// child's lifetime: it LockOSThreads, Starts, Waits, and only
	// exits once the child is reaped — at which point there is no
	// child left to kill when the thread is destroyed.
	//
	// The wait is gated on a `proceed` channel so the main path can
	// register the worker before cleanup runs. Otherwise an immediate
	// exit would run `delete(b.workers, ...)` before the entry exists,
	// and we'd end up with a stale map entry whose port and uid have
	// already been released.
	done := make(chan struct{})
	started := make(chan error, 1)
	proceed := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		// Intentionally do NOT UnlockOSThread. See comment above.

		// Drop the listener and immediately fork. Any code between
		// these two lines extends the window in which another host
		// process can grab the port — keep this pair adjacent.
		_ = ln.Close()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil

		<-proceed

		_ = cmd.Wait()
		close(done)
		// Release the port and UID slots so they can be reused —
		// the worker is no longer holding them. Do NOT delete the
		// map entry: keep it around so Logs() can replay buffered
		// stderr/stdout for diagnosis. The entry persists until an
		// explicit Stop() / RemoveResource() prunes it (matches the
		// Docker semantic where stopped containers remain listable
		// until `docker rm`).
		b.ports.Release(port)
		b.uids.Release(uid)
		slog.Info("process backend: worker exited",
			"worker_id", spec.WorkerID, "port", port, "uid", uid)
	}()

	if err := <-started; err != nil {
		releaseSlots()
		return fmt.Errorf("process backend: start bwrap: %w", err)
	}

	// Ingest stdout and stderr concurrently into the shared log buffer.
	// Two goroutines, not io.MultiReader — MultiReader reads sequentially
	// (stdout to EOF before stderr), which would suppress stderr for the
	// entire worker lifetime.
	go logs.ingest(stdout)
	go logs.ingest(stderr)

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

	// Signal the wait goroutine that registration is done and it is
	// safe to reap+clean up on exit.
	close(proceed)

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

	// If the worker has already exited (e.g., crashed before the
	// caller noticed and called Stop), the wait goroutine has already
	// closed `done` and released slots. We just need to drop the map
	// entry. Skip the SIGTERM-SIGKILL dance for a process that's gone.
	select {
	case <-w.done:
		b.mu.Lock()
		delete(b.workers, id)
		b.mu.Unlock()
		return nil
	default:
	}

	// SIGTERM, then wait up to 10s, then SIGKILL.
	_ = w.process.Signal(syscall.SIGTERM)

	select {
	case <-w.done:
	case <-time.After(10 * time.Second):
		slog.Warn("process backend: worker did not exit after SIGTERM, sending SIGKILL",
			"worker_id", id)
		_ = w.process.Kill()
		<-w.done
	}

	b.mu.Lock()
	delete(b.workers, id)
	b.mu.Unlock()
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
	// Resolve R version so the build uses the same R the worker will.
	if spec.RVersion != "" && len(spec.Cmd) > 0 && spec.Cmd[0] == "R" {
		rPath, fell := ResolveRBinary(spec.RVersion, b.cfg.RPath)
		if fell {
			slog.Warn("requested R version not installed for build, using default",
				"app_id", spec.AppID, "bundle_id", spec.BundleID,
				"requested", spec.RVersion, "fallback", rPath)
		}
		cmd := make([]string, len(spec.Cmd))
		copy(cmd, spec.Cmd)
		cmd[0] = rPath
		spec.Cmd = cmd
	}

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

	secArgs, secCleanup, err := applySeccomp(cmd, b.cfg.SeccompProfile)
	if err != nil {
		return backend.BuildResult{Success: false, ExitCode: 1, Logs: err.Error()}, nil
	}
	defer secCleanup()
	if len(secArgs) > 0 {
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

	// Stream stdout/stderr line-by-line so spec.LogWriter sees output as
	// it arrives — pak builds run for minutes and the build UI renders
	// progress live.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return backend.BuildResult{Success: false, ExitCode: 1, Logs: err.Error()}, nil
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return backend.BuildResult{Success: false, ExitCode: 1, Logs: err.Error()}, nil
	}

	// Run Start+Wait in a dedicated goroutine that LockOSThreads and
	// never unlocks. Same Pdeathsig race as Spawn: pak installs run
	// for minutes, so a stray runtime thread retirement would SIGKILL
	// bwrap mid-build. The ingest goroutines read the pipes from any
	// thread — Pdeathsig only watches the forking thread.
	started := make(chan error, 1)
	waitDone := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Intentionally do NOT UnlockOSThread.
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		waitDone <- cmd.Wait()
	}()

	if err := <-started; err != nil {
		return backend.BuildResult{Success: false, ExitCode: 1, Logs: err.Error()}, nil
	}

	// Two goroutines, one per stream — sequential reads would suppress
	// stderr until stdout closes. A mutex serializes writes into the
	// shared logs buffer and the LogWriter callback so lines are not
	// interleaved mid-character.
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

	waitErr := <-waitDone
	logs := logsBuf.String()

	if waitErr != nil {
		exitCode := 1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
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
	// Kill the process if it's still alive (no SIGTERM grace —
	// RemoveResource is the orphan-cleanup path; force-kill is the
	// expected behavior). Then wait for the wait goroutine to
	// release slots, and prune the map entry.
	select {
	case <-w.done:
	default:
		_ = w.process.Kill()
		<-w.done
	}
	b.mu.Lock()
	delete(b.workers, r.ID)
	b.mu.Unlock()
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

	// Compute CPU percentage from delta ticks / delta wall time, matching
	// the Docker backend's semantics. First call returns 0%.
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
	data, err := os.ReadFile(childrenPath) //nolint:gosec // G304: /proc path
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
	statusData, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)) //nolint:gosec // G304: /proc path
	if err != nil {
		return 0, 0, 0
	}
	rssKB = parseVmRSSKB(string(statusData))

	// CPU from /proc/{pid}/stat (fields 14+15: utime + stime in clock ticks).
	statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)) //nolint:gosec // G304: /proc path
	if err != nil {
		return rssKB, 0, 0
	}
	utime, stime = parseProcStatCPUTicks(string(statData))
	return rssKB, utime, stime
}

// parseVmRSSKB extracts the VmRSS value (in kB) from /proc/<pid>/status
// content. Returns 0 if absent or unparseable.
func parseVmRSSKB(statusContent string) uint64 {
	for _, line := range splitLines(statusContent) {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, _ := strconv.ParseUint(fields[1], 10, 64)
				return v
			}
			return 0
		}
	}
	return 0
}

// parseProcStatCPUTicks extracts utime+stime (clock ticks) from
// /proc/<pid>/stat content. The comm field can contain spaces and
// parens, so we split after the *last* ')'. Returns (0, 0) on
// malformed input.
func parseProcStatCPUTicks(statContent string) (utime, stime uint64) {
	commEnd := strings.LastIndex(statContent, ")")
	if commEnd < 0 || commEnd+2 >= len(statContent) {
		return 0, 0
	}
	fields := strings.Fields(statContent[commEnd+2:])
	// fields[0]=state, [1..]=field4+. utime=field14 → index 11, stime=field15 → index 12.
	if len(fields) > 12 {
		utime, _ = strconv.ParseUint(fields[11], 10, 64)
		stime, _ = strconv.ParseUint(fields[12], 10, 64)
	}
	return utime, stime
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
