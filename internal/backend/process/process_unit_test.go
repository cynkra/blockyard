package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
)

// newFakeBackend constructs a ProcessBackend without going through New()
// — the real New verifies bwrap is on PATH, which is not guaranteed in
// every environment. Tests that only exercise pure helpers or the
// methods that don't fork bwrap use this.
func newFakeBackend(t *testing.T) *ProcessBackend {
	t.Helper()
	cfg := &config.ProcessConfig{
		BwrapPath:      "/nonexistent/bwrap",
		RPath:          "/bin/sh",
		PortRangeStart: 20000,
		PortRangeEnd:   20099,
		WorkerUIDStart: 70000,
		WorkerUIDEnd:   70099,
		WorkerGID:      65534,
	}
	full := &config.Config{Process: cfg}
	return &ProcessBackend{
		cfg:     cfg,
		fullCfg: full,
		ports:   newPortAllocator(cfg.PortRangeStart, cfg.PortRangeEnd),
		uids:    newUIDAllocator(cfg.WorkerUIDStart, cfg.WorkerUIDEnd),
		workers: make(map[string]*workerProc),
	}
}

func TestNewRequiresProcessSection(t *testing.T) {
	_, err := New(&config.Config{})
	if err == nil {
		t.Fatal("expected error when Process config is nil")
	}
	if !strings.Contains(err.Error(), "config section is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewRejectsMissingBwrap(t *testing.T) {
	cfg := &config.Config{
		Process: &config.ProcessConfig{
			BwrapPath:      "/definitely/does/not/exist/bwrap",
			RPath:          "/bin/sh",
			PortRangeStart: 10000,
			PortRangeEnd:   10099,
			WorkerUIDStart: 60000,
			WorkerUIDEnd:   60099,
			WorkerGID:      65534,
		},
	}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected error when bwrap path does not exist")
	}
	if !strings.Contains(err.Error(), "bwrap not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestNewSuccessPath uses /bin/echo as a stand-in for bwrap — New
// only calls exec.LookPath, not the binary itself.
func TestNewSuccessPath(t *testing.T) {
	cfg := &config.Config{
		Storage: config.StorageConfig{BundleWorkerPath: "/tmp/blockyard-test-new"},
		Process: &config.ProcessConfig{
			BwrapPath:      "/bin/echo",
			RPath:          "/bin/sh",
			PortRangeStart: 10000,
			PortRangeEnd:   10099,
			WorkerUIDStart: 60000,
			WorkerUIDEnd:   60099,
			WorkerGID:      65534,
		},
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.cfg != cfg.Process {
		t.Error("backend.cfg not wired from fullCfg.Process")
	}
	if b.fullCfg != cfg {
		t.Error("backend.fullCfg not set")
	}
	if b.ports == nil || b.uids == nil {
		t.Error("allocators not initialized")
	}
	if b.workers == nil {
		t.Error("workers map not initialized")
	}
}

func TestNewRejectsUnreachableBundleMountPoint(t *testing.T) {
	cfg := &config.Config{
		// /proc is read-only, so MkdirAll fails.
		Storage: config.StorageConfig{BundleWorkerPath: "/proc/blockyard-new-test"},
		Process: &config.ProcessConfig{
			BwrapPath:      "/bin/echo",
			RPath:          "/bin/sh",
			PortRangeStart: 10000,
			PortRangeEnd:   10099,
			WorkerUIDStart: 60000,
			WorkerUIDEnd:   60099,
			WorkerGID:      65534,
		},
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error from unreachable bundle mount point")
	}
}

func TestUpdateResourcesReturnsErrNotSupported(t *testing.T) {
	b := newFakeBackend(t)
	err := b.UpdateResources(context.Background(), "irrelevant", 1<<30, 1e9)
	if !errors.Is(err, backend.ErrNotSupported) {
		t.Errorf("expected ErrNotSupported, got %v", err)
	}
}

func TestCleanupOrphanResourcesIsNoop(t *testing.T) {
	b := newFakeBackend(t)
	if err := b.CleanupOrphanResources(context.Background()); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestListManagedEmpty(t *testing.T) {
	b := newFakeBackend(t)
	resources, err := b.ListManaged(context.Background())
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(resources) != 0 {
		t.Errorf("expected empty list, got %d entries", len(resources))
	}
}

func TestListManagedReturnsRegisteredWorker(t *testing.T) {
	b := newFakeBackend(t)
	// Inject a synthetic worker directly into the map. This avoids the
	// bwrap dependency of Spawn while still exercising the list path.
	b.workers["w-1"] = &workerProc{
		port: 20000,
		uid:  70000,
		spec: backend.WorkerSpec{
			WorkerID: "w-1",
			AppID:    "app-1",
			Labels:   map[string]string{"k": "v"},
		},
		done: make(chan struct{}),
	}
	resources, err := b.ListManaged(context.Background())
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(resources) != 1 || resources[0].ID != "w-1" || resources[0].Kind != backend.ResourceContainer {
		t.Errorf("unexpected resources: %+v", resources)
	}
	if resources[0].Labels["k"] != "v" {
		t.Errorf("labels not propagated: %+v", resources[0].Labels)
	}
}

func TestLookupMissingWorker(t *testing.T) {
	b := newFakeBackend(t)
	ctx := context.Background()

	if ok := b.HealthCheck(ctx, "missing"); ok {
		t.Error("HealthCheck should return false for unknown worker")
	}
	if _, err := b.Addr(ctx, "missing"); err == nil {
		t.Error("Addr should return error for unknown worker")
	}
	if _, err := b.Logs(ctx, "missing"); err == nil {
		t.Error("Logs should return error for unknown worker")
	}
	if err := b.Stop(ctx, "missing"); err == nil {
		t.Error("Stop should return error for unknown worker")
	}
	if err := b.RemoveResource(ctx, backend.ManagedResource{ID: "missing"}); err != nil {
		t.Errorf("RemoveResource should tolerate missing: %v", err)
	}
	stats, err := b.WorkerResourceUsage(ctx, "missing")
	if err != nil {
		t.Errorf("WorkerResourceUsage: %v", err)
	}
	if stats != nil {
		t.Errorf("expected nil stats, got %+v", stats)
	}
}

// TestExitedWorkerLogsAreRetained — an exited worker entry persists
// in the map until explicit Stop/RemoveResource so callers can still
// retrieve buffered stderr/stdout for diagnosis (Docker semantic).
func TestExitedWorkerLogsAreRetained(t *testing.T) {
	b := newFakeBackend(t)

	// Inject an exited worker entry directly. Done channel is
	// closed and the log buffer has some content.
	logs := newLogBuffer(10)
	r, w := io.Pipe()
	go logs.ingest(r)
	fmt.Fprintln(w, "hello")
	fmt.Fprintln(w, "goodbye")
	w.Close()
	waitLogBufferClosed(t, logs)

	done := make(chan struct{})
	close(done)
	b.workers["exited-worker"] = &workerProc{
		port: 20000,
		uid:  70000,
		spec: backend.WorkerSpec{WorkerID: "exited-worker"},
		logs: logs,
		done: done,
	}

	ctx := context.Background()

	// Logs() must succeed and return the buffered content.
	stream, err := b.Logs(ctx, "exited-worker")
	if err != nil {
		t.Fatalf("Logs after exit: %v", err)
	}
	var lines []string
	for line := range stream.Lines {
		lines = append(lines, line)
	}
	if len(lines) != 2 || lines[0] != "hello" || lines[1] != "goodbye" {
		t.Errorf("unexpected log lines: %v", lines)
	}

	// HealthCheck must report unhealthy (done channel is closed).
	if b.HealthCheck(ctx, "exited-worker") {
		t.Error("HealthCheck should return false for an exited worker")
	}

	// Addr should still return the last-known address (matches the
	// Docker semantic where you can inspect a stopped container).
	addr, err := b.Addr(ctx, "exited-worker")
	if err != nil {
		t.Errorf("Addr: %v", err)
	}
	if addr == "" {
		t.Error("Addr returned empty string")
	}

	// ListManaged includes the exited worker.
	resources, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, r := range resources {
		if r.ID == "exited-worker" {
			seen = true
		}
	}
	if !seen {
		t.Error("ListManaged should include exited worker")
	}

	// Stop on an exited worker is a no-op + delete.
	if err := b.Stop(ctx, "exited-worker"); err != nil {
		t.Errorf("Stop on exited worker: %v", err)
	}

	// After Stop, the entry is gone.
	if _, err := b.Logs(ctx, "exited-worker"); err == nil {
		t.Error("Logs should fail after Stop deletes the entry")
	}
}

// TestSpawnErrorPathsReleaseSlots asserts that every Spawn failure
// returns both pools to their pre-call state. Without this, repeated
// failed spawns would leak the finite port/UID pools.
func TestSpawnErrorPathsReleaseSlots(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*ProcessBackend)
		wantErr  string
	}{
		{
			name: "seccomp_open_fails",
			setup: func(b *ProcessBackend) {
				b.cfg.SeccompProfile = "/nonexistent/seccomp.bpf"
			},
			wantErr: "seccomp",
		},
		{
			name: "port_pool_exhausted",
			setup: func(b *ProcessBackend) {
				for {
					_, ln, err := b.ports.Reserve()
					if err != nil {
						return
					}
					// We only need the bitset to be marked; the
					// listener is incidental for this test.
					ln.Close()
				}
			},
			wantErr: "ports in use",
		},
		{
			name: "uid_pool_exhausted",
			setup: func(b *ProcessBackend) {
				for {
					if _, err := b.uids.Alloc(); err != nil {
						break
					}
				}
			},
			wantErr: "UIDs in use",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newFakeBackend(t)
			tc.setup(b)
			beforePorts := b.ports.InUse()
			beforeUIDs := b.uids.InUse()

			err := b.Spawn(context.Background(), backend.WorkerSpec{
				WorkerID:    "w",
				BundlePath:  t.TempDir(),
				WorkerMount: "/tmp/app",
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tc.wantErr)
			}
			if b.ports.InUse() != beforePorts {
				t.Errorf("port slots leaked: before=%d after=%d",
					beforePorts, b.ports.InUse())
			}
			if b.uids.InUse() != beforeUIDs {
				t.Errorf("UID slots leaked: before=%d after=%d",
					beforeUIDs, b.uids.InUse())
			}
		})
	}
}

// TestSpawnRefusesDuplicateLiveWorker verifies that calling Spawn
// with a worker ID that's already running returns an error rather
// than clobbering the live entry.
func TestSpawnRefusesDuplicateLiveWorker(t *testing.T) {
	b := newFakeBackend(t)

	// Inject a "live" entry (done channel still open).
	b.workers["live-worker"] = &workerProc{
		port: 20000,
		uid:  70000,
		spec: backend.WorkerSpec{WorkerID: "live-worker"},
		done: make(chan struct{}),
	}

	// Spawn would normally fail at the bwrap LookPath step (the
	// fake backend uses /nonexistent/bwrap), but the duplicate
	// check runs first so we should see the duplicate error.
	err := b.Spawn(context.Background(), backend.WorkerSpec{WorkerID: "live-worker"})
	if err == nil {
		t.Fatal("expected error spawning duplicate live worker")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReadOneProcStatsSelf(t *testing.T) {
	// Reading our own PID should always succeed and return non-zero
	// values for a running test binary.
	pid := os.Getpid()
	rssKB, utime, stime := readOneProcStats(pid)
	if rssKB == 0 {
		t.Error("expected non-zero RSS for self")
	}
	// utime+stime may legitimately be 0 for a just-started goroutine,
	// but on a test runner something has already accounted. Be lenient.
	_ = utime
	_ = stime
}

func TestReadOneProcStatsMissing(t *testing.T) {
	// PID 0 never exists; expect all-zero result.
	rssKB, utime, stime := readOneProcStats(0)
	if rssKB != 0 || utime != 0 || stime != 0 {
		t.Errorf("expected zeros for missing pid, got %d %d %d", rssKB, utime, stime)
	}
}

func TestReadProcTreeStatsSelf(t *testing.T) {
	// Tree stats for self should at least return non-zero RSS.
	rssBytes, _ := readProcTreeStats(os.Getpid())
	if rssBytes == 0 {
		t.Error("expected non-zero tree RSS for self")
	}
}

func TestCollectDescendantsSelf(t *testing.T) {
	// The test process may or may not have children; either case is
	// fine, we just verify the call doesn't error or panic.
	_ = collectDescendants(os.Getpid())
}

func TestEnsureBundleMountPointEmpty(t *testing.T) {
	if err := ensureBundleMountPoint(""); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestEnsureBundleMountPointRelative(t *testing.T) {
	if err := ensureBundleMountPoint("relative/path"); err == nil {
		t.Error("expected error for relative path")
	}
}

func TestEnsureBundleMountPointUnderTmp(t *testing.T) {
	// Anything under /tmp is handled by the in-sandbox tmpfs and
	// requires no host setup. ensureBundleMountPoint should accept
	// it without touching the host filesystem, even if the path
	// doesn't exist.
	if err := ensureBundleMountPoint("/tmp/blockyard-nonexistent-test-path"); err != nil {
		t.Errorf("unexpected error for /tmp path: %v", err)
	}
	if _, err := os.Stat("/tmp/blockyard-nonexistent-test-path"); !os.IsNotExist(err) {
		t.Error("ensureBundleMountPoint must not touch the host for /tmp paths")
		os.RemoveAll("/tmp/blockyard-nonexistent-test-path")
	}
}

// nonTmpTempDir creates a temp directory NOT under /tmp so tests
// exercise the host-mkdir branch of ensureBundleMountPoint instead
// of the /tmp shortcut.
func nonTmpTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "blockyard-process-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestEnsureBundleMountPointExisting(t *testing.T) {
	dir := nonTmpTempDir(t)
	if err := ensureBundleMountPoint(dir); err != nil {
		t.Errorf("unexpected error for existing dir: %v", err)
	}
}

func TestEnsureBundleMountPointExistingNotDir(t *testing.T) {
	dir := nonTmpTempDir(t)
	notDir := dir + "/file"
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureBundleMountPoint(notDir); err == nil {
		t.Error("expected error when path exists as a file, not a directory")
	}
}

func TestEnsureBundleMountPointCreatesNew(t *testing.T) {
	dir := nonTmpTempDir(t)
	target := dir + "/new/nested/dir"
	if err := ensureBundleMountPoint(target); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("expected directory to exist: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected created path to be a directory")
	}
}

func TestEnsureBundleMountPointCreationFails(t *testing.T) {
	// Try to create a directory under a path that's read-only or
	// nonexistent at a parent that we can't write to. /proc is the
	// canonical "you can't write here" path.
	if err := ensureBundleMountPoint("/proc/blockyard-test-nonexistent"); err == nil {
		t.Error("expected error creating directory under /proc")
	}
}

// startBackgroundCmd spawns a command, registers a cleanup that
// kills and reaps it, and returns the *exec.Cmd. Skips the test if
// the binary cannot be launched.
func startBackgroundCmd(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn %s: %v", name, err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd
}

// injectLiveWorker spawns a long-running /bin/sleep, wires the
// done-channel reaper goroutine (mirroring what Spawn's real wait
// goroutine does), and registers the synthetic workerProc under id.
// Returns the done channel so callers can assert reap ordering.
// The injected workerProc has cmd + process populated so Stop,
// RemoveResource, and WorkerResourceUsage all work end-to-end.
func injectLiveWorker(t *testing.T, b *ProcessBackend, id string) chan struct{} {
	t.Helper()
	cmd := startBackgroundCmd(t, "/bin/sleep", "30s")
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	b.workers[id] = &workerProc{
		cmd:     cmd,
		process: cmd.Process,
		spec:    backend.WorkerSpec{WorkerID: id},
		done:    done,
	}
	return done
}

// TestHealthCheckLiveListener exercises the DialContext branch;
// other HealthCheck tests only cover unknown/exited.
func TestHealthCheckLiveListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	b := newFakeBackend(t)
	port := ln.Addr().(*net.TCPAddr).Port
	b.workers["live"] = &workerProc{
		port: port,
		uid:  70000,
		spec: backend.WorkerSpec{WorkerID: "live"},
		done: make(chan struct{}),
	}
	if !b.HealthCheck(context.Background(), "live") {
		t.Error("expected healthy listener to report healthy")
	}
}

func TestHealthCheckListenerClosed(t *testing.T) {
	// Acquire an ephemeral port, then release it — nothing will be
	// listening when HealthCheck dials.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	b := newFakeBackend(t)
	b.workers["zombie"] = &workerProc{
		port: port,
		spec: backend.WorkerSpec{WorkerID: "zombie"},
		done: make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if b.HealthCheck(ctx, "zombie") {
		t.Error("expected dial failure to report unhealthy")
	}
}

func TestStopSendsSIGTERM(t *testing.T) {
	b := newFakeBackend(t)
	done := injectLiveWorker(t, b, "s1")

	if err := b.Stop(context.Background(), "s1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := b.workers["s1"]; ok {
		t.Error("worker still in map after Stop")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("process did not exit after Stop")
	}
}

func TestRemoveResourceKillsLiveProcess(t *testing.T) {
	b := newFakeBackend(t)
	done := injectLiveWorker(t, b, "orphan")

	if err := b.RemoveResource(context.Background(),
		backend.ManagedResource{ID: "orphan"}); err != nil {
		t.Fatalf("RemoveResource: %v", err)
	}
	if _, ok := b.workers["orphan"]; ok {
		t.Error("entry still in map after RemoveResource")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("process did not exit after RemoveResource")
	}
}

// TestRemoveResourceAlreadyExited covers the fast-path branch where
// done is already closed: no kill, no wait, just the map delete.
func TestRemoveResourceAlreadyExited(t *testing.T) {
	b := newFakeBackend(t)
	done := make(chan struct{})
	close(done)
	b.workers["dead"] = &workerProc{
		spec: backend.WorkerSpec{WorkerID: "dead"},
		done: done,
	}
	if err := b.RemoveResource(context.Background(),
		backend.ManagedResource{ID: "dead"}); err != nil {
		t.Errorf("RemoveResource: %v", err)
	}
	if _, ok := b.workers["dead"]; ok {
		t.Error("entry not removed")
	}
}

// TestWorkerResourceUsageLiveChild calls WorkerResourceUsage twice:
// the first call takes the no-previous-sample branch (CPUPercent=0),
// the second call takes the delta branch (lastCPU non-nil).
func TestWorkerResourceUsageLiveChild(t *testing.T) {
	b := newFakeBackend(t)
	injectLiveWorker(t, b, "w")
	ctx := context.Background()

	first, err := b.WorkerResourceUsage(ctx, "w")
	if err != nil {
		t.Fatalf("first WorkerResourceUsage: %v", err)
	}
	if first == nil {
		t.Fatal("first call returned nil stats")
		return // unreachable; satisfies staticcheck SA5011
	}
	if first.MemoryUsageBytes == 0 {
		t.Error("expected non-zero RSS for a live process")
	}
	if first.MemoryLimitBytes != 0 {
		t.Errorf("expected 0 memory limit, got %d", first.MemoryLimitBytes)
	}
	if first.CPUPercent != 0 {
		t.Errorf("first call should report 0%% CPU, got %v", first.CPUPercent)
	}

	second, err := b.WorkerResourceUsage(ctx, "w")
	if err != nil {
		t.Fatalf("second WorkerResourceUsage: %v", err)
	}
	if second == nil {
		t.Fatal("second call returned nil stats")
		return // unreachable; satisfies staticcheck SA5011
	}
	// sleep consumes ~0 CPU; value must be a finite, sane number.
	if second.CPUPercent < 0 || second.CPUPercent > 100 {
		t.Errorf("unexpected CPU%% on second call: %v", second.CPUPercent)
	}
}

// TestCollectDescendantsWithChild verifies collectDescendants walks
// the /proc children tree; the pre-existing TestCollectDescendantsSelf
// only checks that the call doesn't panic.
func TestCollectDescendantsWithChild(t *testing.T) {
	cmd := startBackgroundCmd(t, "/bin/sh", "-c", "sleep 30 & wait")

	// Poll for the forked sleep — the shell may not have exec'd it yet
	// when we return from Start, so a fixed sleep would race under load.
	deadline := time.Now().Add(2 * time.Second)
	var kids []int
	for time.Now().Before(deadline) {
		kids = collectDescendants(cmd.Process.Pid)
		if len(kids) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("expected at least one descendant for /bin/sh child")
}

// TestPreflightDelegates asserts that the backend's Preflight method
// dispatches RunPreflight and returns the expected set of check
// names. Catches regressions in the delegation wiring.
func TestPreflightDelegates(t *testing.T) {
	b := newFakeBackend(t)
	report, err := b.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if report == nil {
		t.Fatal("expected non-nil report")
		return // unreachable; satisfies staticcheck SA5011
	}
	wantNames := map[string]bool{
		"bwrap_available":        false,
		"r_binary":               false,
		"user_namespaces":        false,
		"port_range":             false,
		"resource_limits":        false,
		"bwrap_host_uid_mapping": false,
		"worker_egress":          false,
	}
	for _, r := range report.Results {
		if _, ok := wantNames[r.Name]; ok {
			wantNames[r.Name] = true
		}
	}
	for name, seen := range wantNames {
		if !seen {
			t.Errorf("Preflight report missing %q", name)
		}
	}
}

func TestParseVmRSSKB(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want uint64
	}{
		{"present", "Name:\tsh\nVmRSS:\t1234 kB\nFoo: bar", 1234},
		{"present_zero", "VmRSS:\t0 kB", 0},
		{"absent", "Name:\tsh\nVmPeak:\t9999 kB", 0},
		{"empty", "", 0},
		{"malformed_one_field", "VmRSS:", 0},
		{"malformed_non_numeric", "VmRSS:\tnope kB", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseVmRSSKB(tc.in)
			if got != tc.want {
				t.Errorf("parseVmRSSKB(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseProcStatCPUTicks covers the pure CPU-tick parser. The
// comm field (field 2) is parenthesized and may contain spaces or
// parens, so the parser must split after the LAST ')'. utime/stime
// are fields 14/15 (index 11/12 after dropping pid+comm).
func TestParseProcStatCPUTicks(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantU, wantS uint64
	}{
		{
			name: "simple",
			in:   "1 (sh) S 0 0 0 0 0 0 0 0 0 0 111 222 0 0 0 0 0 0 0 0\n",
			wantU: 111, wantS: 222,
		},
		{
			name: "comm_with_space",
			in:   "42 (sh weird) R 0 0 0 0 0 0 0 0 0 0 17 19 0 0 0 0 0 0 0 0\n",
			wantU: 17, wantS: 19,
		},
		{
			name: "comm_with_parens",
			in:   "99 (ev(il)) S 0 0 0 0 0 0 0 0 0 0 3 5 0 0 0 0 0 0 0 0\n",
			wantU: 3, wantS: 5,
		},
		{
			name: "no_closing_paren",
			in:   "42 (never-closed",
			// malformed — both return 0
		},
		{
			name: "too_short_after_comm",
			in:   "1 (sh) S",
			// only state — no tick fields — both return 0
		},
		{
			name: "empty",
			in:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotU, gotS := parseProcStatCPUTicks(tc.in)
			if gotU != tc.wantU || gotS != tc.wantS {
				t.Errorf("parseProcStatCPUTicks(%q) = (%d, %d), want (%d, %d)",
					tc.in, gotU, gotS, tc.wantU, tc.wantS)
			}
		})
	}
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a\n", []string{"a"}},
		{"a\nb", []string{"a", "b"}},
		{"a\nb\n", []string{"a", "b"}},
		{"\n\n", nil},
		{"a\n\nb", []string{"a", "b"}},
	}
	for _, tc := range cases {
		got := splitLines(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitLines(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitLines(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
