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
	if cfg == nil {
		return nil, fmt.Errorf("process backend: [process] config section is required")
	}
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

// Preflight implements backend.Backend by delegating to RunPreflight.
func (b *ProcessBackend) Preflight(_ context.Context) (*preflight.Report, error) {
	return RunPreflight(b.cfg, b.fullCfg), nil
}

// CleanupOrphanResources implements backend.Backend. The process
// backend tracks workers in memory only — Pdeathsig + --die-with-parent
// ensure no orphans survive a server restart, so there is nothing to
// clean up.
func (b *ProcessBackend) CleanupOrphanResources(_ context.Context) error {
	return nil
}

func (b *ProcessBackend) Spawn(_ context.Context, spec backend.WorkerSpec) error {
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
	statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)) //nolint:gosec // G304: /proc path
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
