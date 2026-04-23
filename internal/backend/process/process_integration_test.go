//go:build process_test

package process_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/process"
	"github.com/cynkra/blockyard/internal/config"
)

// bwrapMode classifies what the current test process can reasonably
// exercise. Since #305, host-effective worker UIDs only happen when
// blockyard runs as root: the spawn path fork+setuid's the child to
// the worker UID before exec(bwrap), giving bwrap caller_uid ==
// sandbox_uid and therefore an identity uid_map. Non-root blockyard
// (CI's `setuid` and `unprivileged` matrices) cannot do this, so the
// sandboxed child's kuid in init_userns stays at blockyard's own UID;
// lifecycle tests that assume workers appear as distinct host UIDs
// have to skip.
//
//   - bwrapHostMapped: blockyard is root, fork+setuid path works.
//   - bwrapNoHostMap: blockyard is non-root; iptables owner-match
//     isolation is unavailable until phase 3-9 lands --userns +
//     newuidmap.
//   - bwrapUnavailable: bwrap is missing or can't create a user
//     namespace at all; every process_test integration test skips.
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
	if err := exec.Command("bwrap",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-pid", "--unshare-user", "--unshare-uts",
		"--die-with-parent", "--new-session",
		"--", "/bin/true").Run(); err != nil {
		return bwrapUnavailable
	}
	if os.Getuid() != 0 {
		return bwrapNoHostMap
	}
	return bwrapHostMapped
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

// requireHostUIDMapping skips the test unless the spawn path can
// produce a sandboxed child whose kuid in init_userns matches the
// requested worker UID. Post-#305 this requires blockyard to run as
// root (fork+setuid before exec(bwrap) → identity uid_map). Non-root
// blockyard keeps sandboxed children at blockyard's own kuid, which
// makes any test that depends on per-worker host identity
// meaningless.
func requireHostUIDMapping(t *testing.T) {
	t.Helper()
	switch detectBwrapMode(t) {
	case bwrapUnavailable:
		t.Skip("bwrap not available")
	case bwrapNoHostMap:
		t.Skip("non-root blockyard: spawn path cannot produce identity uid_map until phase 3-9 ships --userns+newuidmap")
	}
}

// workerAccessibleTempDir wraps t.TempDir() and makes the returned
// directory (and each tmpdir-prefixed ancestor) traversable by the
// worker UID. t.TempDir mkdirs each segment 0700 owned by the caller
// (root in the root-blockyard matrix); post-#305 workers run as
// WorkerUIDStart inside the bwrap sandbox after a fork+setuid, so
// bwrap's bind-mount of a 0700 root-owned BundlePath fails with
// "Can't find source path". Production deployments put bundles in
// world-readable storage; tests need to match that constraint.
func workerAccessibleTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := dir
	for {
		if p == "/" || p == filepath.Clean(os.TempDir()) || !strings.HasPrefix(p, os.TempDir()+string(os.PathSeparator)) {
			break
		}
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatalf("chmod %s: %v", p, err)
		}
		p = filepath.Dir(p)
	}
	return dir
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
	be, err := process.New(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	spec := backend.WorkerSpec{
		WorkerID:    "test-worker-1",
		BundlePath:  workerAccessibleTempDir(t),
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
	be, err := process.New(cfg, nil, nil)
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
	be, err := process.New(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	spec := backend.WorkerSpec{
		WorkerID:    "stats-worker",
		BundlePath:  workerAccessibleTempDir(t),
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
	be, err := process.New(cfg, nil, nil)
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
	be, err := process.New(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	spec := backend.WorkerSpec{
		WorkerID:    "r-smoke",
		BundlePath:  workerAccessibleTempDir(t),
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
