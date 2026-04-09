//go:build process_test

package process_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/process"
	"github.com/cynkra/blockyard/internal/config"
)

// bwrapMode classifies the bwrap deployment mode the current test
// process is running under. The three modes correspond directly to
// the deployment modes documented in backends.md / phase-3-7.md:
//
//   - bwrapHostMapped — bwrap can write a uid_map that gives the
//     sandboxed child a host-effective UID different from the
//     caller's UID. This is what happens when blockyard runs as
//     root (containerized mode) or when bwrap is setuid root
//     (Fedora/RHEL default). Both spawn lifecycle tests and the
//     uid-mapping preflight check should succeed.
//
//   - bwrapNoHostMap — bwrap can spawn but cannot write a uid_map
//     to a foreign UID. The unprivileged kernel.userns_clone path
//     where workers all end up running as the caller's host UID,
//     defeating the iptables --uid-owner egress isolation. The
//     preflight check is supposed to *catch* this — tests assert
//     it does — and lifecycle tests skip because every spawn
//     would fail with "setting up uid map: Permission denied".
//
//   - bwrapUnavailable — bwrap is missing, or bwrap can't even
//     create a user namespace (no unprivileged userns at all).
//     All process_test integration tests skip.
type bwrapMode int

const (
	bwrapUnavailable bwrapMode = iota
	bwrapNoHostMap
	bwrapHostMapped
)

// detectBwrapMode probes the current bwrap configuration. It always
// returns one of the three modes; cached so the probe runs once per
// test process.
var detectedBwrapMode struct {
	once sync.Once
	mode bwrapMode
}

func detectBwrapMode(t *testing.T) bwrapMode {
	t.Helper()
	detectedBwrapMode.once.Do(func() {
		detectedBwrapMode.mode = probeBwrapMode()
	})
	return detectedBwrapMode.mode
}

func probeBwrapMode() bwrapMode {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return bwrapUnavailable
	}
	// Step 1: can bwrap even create a user namespace? Without --uid,
	// bwrap maps the caller's UID to itself in the new namespace,
	// which works on every kernel that allows unprivileged userns.
	if err := exec.Command("bwrap",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-pid", "--unshare-user", "--unshare-uts",
		"--die-with-parent", "--new-session",
		"--", "/bin/true").Run(); err != nil {
		return bwrapUnavailable
	}
	// Step 2: can bwrap write a uid_map to a UID distinct from the
	// caller's? Pick a probe UID guaranteed to differ.
	probeUID := os.Getuid() + 12345
	if probeUID == os.Getuid() { // overflow guard, never happens in practice
		probeUID++
	}
	cmd := exec.Command("bwrap",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-pid", "--unshare-user", "--unshare-uts",
		"--uid", strconv.Itoa(probeUID),
		"--gid", "65534",
		"--die-with-parent", "--new-session",
		"--", "/bin/sleep", "0.5")
	if err := cmd.Start(); err != nil {
		return bwrapNoHostMap
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	// Read /proc/<pid>/status from the parent's view to confirm the
	// host-effective UID matches what we asked for.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", cmd.Process.Pid))
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "Uid:") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return bwrapNoHostMap
			}
			realUID, perr := strconv.Atoi(fields[1])
			if perr != nil {
				return bwrapNoHostMap
			}
			if realUID == probeUID {
				return bwrapHostMapped
			}
			return bwrapNoHostMap
		}
		time.Sleep(20 * time.Millisecond)
	}
	return bwrapNoHostMap
}

// requireBwrap skips the test if bwrap is missing or can't even
// create a user namespace. Use this for tests that just need bwrap
// to spawn — they don't depend on the host UID mapping mode.
func requireBwrap(t *testing.T) {
	t.Helper()
	if detectBwrapMode(t) == bwrapUnavailable {
		t.Skip("bwrap not available or unprivileged userns disabled")
	}
}

// requireHostUIDMapping skips the test unless bwrap can produce a
// host-effective UID different from the caller's. Lifecycle tests
// that spawn workers via the configured WorkerUID range need this;
// in mode (c) every spawn would fail with "setting up uid map:
// Permission denied".
func requireHostUIDMapping(t *testing.T) {
	t.Helper()
	switch detectBwrapMode(t) {
	case bwrapUnavailable:
		t.Skip("bwrap not available")
	case bwrapNoHostMap:
		t.Skip("bwrap cannot write a host-effective uid_map (run as root, install bwrap setuid, or set up subuid ranges)")
	}
}

func TestSpawnAndStop(t *testing.T) {
	requireHostUIDMapping(t)

	cfg := &config.Config{
		Server:  config.ServerConfig{Backend: "process"},
		Storage: config.StorageConfig{BundleWorkerPath: "/tmp/app"},
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
	be, err := process.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	spec := backend.WorkerSpec{
		WorkerID:    "test-worker-1",
		BundlePath:  t.TempDir(),
		WorkerMount: "/tmp/app",
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
		Server:  config.ServerConfig{Backend: "process"},
		Storage: config.StorageConfig{BundleWorkerPath: "/tmp/app"},
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
	be, err := process.New(cfg, nil)
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
	requireHostUIDMapping(t)

	cfg := &config.Config{
		Server:  config.ServerConfig{Backend: "process"},
		Storage: config.StorageConfig{BundleWorkerPath: "/tmp/app"},
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
	be, err := process.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	spec := backend.WorkerSpec{
		WorkerID:    "stats-worker",
		BundlePath:  t.TempDir(),
		WorkerMount: "/tmp/app",
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

func TestUpdateResourcesNotSupported(t *testing.T) {
	requireBwrap(t)
	cfg := &config.Config{
		Server:  config.ServerConfig{Backend: "process"},
		Storage: config.StorageConfig{BundleWorkerPath: "/tmp/app"},
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
	be, err := process.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	err = be.UpdateResources(context.Background(), "irrelevant", 1<<30, 1e9)
	if err != backend.ErrNotSupported {
		t.Errorf("expected ErrNotSupported, got %v", err)
	}
}

// TestRSmokeBoot is a minimal end-to-end check that R can actually
// run inside the bwrap sandbox the process backend constructs. None
// of the lifecycle tests above exercise the real default Cmd path
// (R + shiny::runApp); they all stub R with /bin/sleep. This test
// runs `R --version` instead of a Shiny app — just enough to verify
// that the bwrap mounts let the R binary find its libraries, that
// the env var setup is sane, and that the constructed args don't
// silently break the R startup. Phase 3-8 will add a proper Shiny
// boot test once it ships the production worker image with pak; for
// now this catches the most likely regression class (broken arg
// construction, missing system mounts, env var typos).
func TestRSmokeBoot(t *testing.T) {
	requireHostUIDMapping(t)
	rPath, err := exec.LookPath("R")
	if err != nil {
		t.Skip("R not installed; skipping smoke test")
	}

	cfg := &config.Config{
		Server:  config.ServerConfig{Backend: "process"},
		Storage: config.StorageConfig{BundleWorkerPath: "/tmp/app"},
		Process: &config.ProcessConfig{
			BwrapPath:      "bwrap",
			RPath:          rPath,
			PortRangeStart: 19400,
			PortRangeEnd:   19499,
			WorkerUIDStart: 69400,
			WorkerUIDEnd:   69499,
			WorkerGID:      65534,
		},
	}
	be, err := process.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	spec := backend.WorkerSpec{
		WorkerID:    "r-smoke",
		BundlePath:  t.TempDir(),
		WorkerMount: "/tmp/app",
		Cmd:         []string{rPath, "--version"},
	}

	// Open the log stream BEFORE Spawn so we capture R's stderr from
	// the start (R prints version info and exits in well under a
	// second; the integration test's other path of "Logs() after
	// Stop()" wouldn't catch it).
	if err := be.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer be.Stop(ctx, spec.WorkerID)

	stream, err := be.Logs(ctx, spec.WorkerID)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer stream.Close()

	// R --version exits in milliseconds. Wait for the wait goroutine
	// to reap the process so the log buffer is closed and the stream
	// drain returns; otherwise we'd block waiting for more lines that
	// never come.
	deadline := time.After(10 * time.Second)
	var lines []string
	collecting := true
	for collecting {
		select {
		case line, ok := <-stream.Lines:
			if !ok {
				collecting = false
				break
			}
			lines = append(lines, line)
		case <-deadline:
			t.Fatalf("timed out waiting for R output; collected so far: %v", lines)
		}
	}

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "R version") {
		t.Errorf("expected output to contain 'R version', got:\n%s", joined)
	}
}
