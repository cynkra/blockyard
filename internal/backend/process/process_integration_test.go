//go:build process_test

package process_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/process"
	"github.com/cynkra/blockyard/internal/config"
)

// requireBwrap skips the test if bwrap is not installed or cannot
// create user namespaces in the current environment (some kernels and
// some hosted CI runners disable unprivileged userns). Distinguishing
// "absent" from "broken" lets us run tests on developer machines that
// have a working bwrap and skip cleanly on the CI image.
func requireBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not available")
	}
	// Smoke test: try a minimal sandbox. If it fails because the kernel
	// disallows unprivileged user namespaces, skip rather than fail.
	out, err := exec.Command("bwrap",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-pid", "--unshare-user", "--unshare-uts",
		"--die-with-parent", "--new-session",
		"--", "/bin/true").CombinedOutput()
	if err != nil {
		t.Skipf("bwrap not functional in this environment: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

func TestSpawnAndStop(t *testing.T) {
	requireBwrap(t)

	cfg := &config.Config{
		Server: config.ServerConfig{Backend: "process"},
		Process: &config.ProcessConfig{
			BwrapPath:      "bwrap",
			RPath:          "/bin/sleep", // sleep stands in for R in lifecycle tests
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
		Cmd:         []string{"/bin/sleep", "60"},
	}

	if err := be.Spawn(ctx, spec); err != nil {
		t.Fatal(err)
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
	requireBwrap(t)
	cfg := &config.Config{
		Server: config.ServerConfig{Backend: "process"},
		Process: &config.ProcessConfig{
			BwrapPath:      "bwrap",
			RPath:          "/bin/sleep",
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

	stats, err := be.WorkerResourceUsage(context.Background(), "nonexistent")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if stats != nil {
		t.Errorf("expected nil stats for unknown worker, got %+v", stats)
	}
}

func TestWorkerResourceUsageLiveWorker(t *testing.T) {
	requireBwrap(t)

	cfg := &config.Config{
		Server: config.ServerConfig{Backend: "process"},
		Process: &config.ProcessConfig{
			BwrapPath:      "bwrap",
			RPath:          "/bin/sleep",
			PortRangeStart: 19200,
			PortRangeEnd:   19299,
			WorkerUIDStart: 69200,
			WorkerUIDEnd:   69299,
			WorkerGID:      65534,
		},
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
		Cmd:         []string{"/bin/sleep", "60"},
	}
	if err := be.Spawn(ctx, spec); err != nil {
		t.Fatal(err)
	}
	defer be.Stop(ctx, spec.WorkerID)

	// Give the sandboxed process a moment to start and allocate RSS.
	time.Sleep(200 * time.Millisecond)

	// If the worker is gone already the test is going to fail; grab
	// the buffered bwrap logs before that so we can see *why*.
	dumpLogsOnFail := func() {
		stream, lerr := be.Logs(ctx, spec.WorkerID)
		if lerr != nil {
			t.Logf("Logs() after worker exit: %v", lerr)
			return
		}
		defer stream.Close()
		for line := range stream.Lines {
			t.Logf("worker log: %s", line)
		}
	}

	stats, err := be.WorkerResourceUsage(ctx, spec.WorkerID)
	if err != nil {
		dumpLogsOnFail()
		t.Fatalf("expected no error, got %v", err)
	}
	if stats == nil {
		dumpLogsOnFail()
		t.Fatal("expected non-nil stats for live worker")
	}
	if stats.MemoryUsageBytes == 0 {
		t.Error("expected non-zero RSS for a live process")
	}
	if stats.MemoryLimitBytes != 0 {
		t.Errorf("expected 0 memory limit (no cgroup), got %d", stats.MemoryLimitBytes)
	}
}

func TestUpdateResourcesNotSupported(t *testing.T) {
	requireBwrap(t)
	cfg := &config.Config{
		Server: config.ServerConfig{Backend: "process"},
		Process: &config.ProcessConfig{
			BwrapPath:      "bwrap",
			RPath:          "/bin/sleep",
			PortRangeStart: 19300,
			PortRangeEnd:   19399,
			WorkerUIDStart: 69300,
			WorkerUIDEnd:   69399,
			WorkerGID:      65534,
		},
	}
	be, err := process.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	err = be.UpdateResources(context.Background(), "irrelevant", 1<<30, 1e9)
	if err != backend.ErrNotSupported {
		t.Errorf("expected ErrNotSupported, got %v", err)
	}
}
