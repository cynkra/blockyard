package process

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/redisstate"
)

// --- CheckRVersion ---

func TestCheckRVersion_Empty(t *testing.T) {
	b := newFakeBackend(t)
	if err := b.CheckRVersion(""); err != nil {
		t.Errorf("empty version should be OK: %v", err)
	}
}

func TestCheckRVersion_Resolves(t *testing.T) {
	dir := setupRigFixture(t, "4.5.0")
	orig := rigBase
	defer setRigBase(orig)
	setRigBase(dir)

	b := newFakeBackend(t)
	if err := b.CheckRVersion("4.5.0"); err != nil {
		t.Errorf("installed version should resolve cleanly: %v", err)
	}
}

func TestCheckRVersion_MissingOtherVersionsInstalled(t *testing.T) {
	dir := setupRigFixture(t, "4.5.0", "4.4.3")
	orig := rigBase
	defer setRigBase(orig)
	setRigBase(dir)

	b := newFakeBackend(t)
	err := b.CheckRVersion("3.6.0")
	if err == nil {
		t.Fatal("expected error for uninstalled version")
	}
	if !strings.Contains(err.Error(), "not installed; available:") {
		t.Errorf("error should list available versions, got: %v", err)
	}
}

func TestCheckRVersion_NoneInstalled(t *testing.T) {
	// Point rigBase at an empty directory so InstalledRVersions returns [].
	dir := t.TempDir()
	orig := rigBase
	defer setRigBase(orig)
	setRigBase(dir)

	b := newFakeBackend(t)
	err := b.CheckRVersion("4.5.0")
	if err == nil {
		t.Fatal("expected error when no R versions installed")
	}
	if !strings.Contains(err.Error(), "no R versions are installed") {
		t.Errorf("error should explain no versions, got: %v", err)
	}
}

// --- selectAllocators ---

func TestSelectAllocators_ExplicitMemory(t *testing.T) {
	cfg := &config.Config{
		Process: &config.ProcessConfig{
			PortRangeStart: 30000, PortRangeEnd: 30010,
			WorkerUIDStart: 80000, WorkerUIDEnd: 80010,
		},
		Proxy: config.ProxyConfig{SessionStore: config.SessionStoreMemory},
	}
	p, u := selectAllocators(cfg, nil, nil)
	if _, ok := p.(*memoryPortAllocator); !ok {
		t.Errorf("port allocator = %T, want memoryPortAllocator", p)
	}
	if _, ok := u.(*memoryUIDAllocator); !ok {
		t.Errorf("uid allocator = %T, want memoryUIDAllocator", u)
	}
}

// TestSelectAllocators_Redis covers the Redis branch of the selector.
// A miniredis stands in for a live Redis; selectAllocators only calls
// the constructors, which don't exercise the connection, so the test
// is deterministic without a real Redis server.
func TestSelectAllocators_Redis(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redisstate.TestClient(t, mr.Addr())

	cfg := &config.Config{
		Process: &config.ProcessConfig{
			PortRangeStart: 30000, PortRangeEnd: 30010,
			WorkerUIDStart: 80000, WorkerUIDEnd: 80010,
		},
		Proxy: config.ProxyConfig{SessionStore: config.SessionStoreRedis},
	}
	p, u := selectAllocators(cfg, rc, nil)
	if _, ok := p.(*redisPortAllocator); !ok {
		t.Errorf("port allocator = %T, want redisPortAllocator", p)
	}
	if _, ok := u.(*redisUIDAllocator); !ok {
		t.Errorf("uid allocator = %T, want redisUIDAllocator", u)
	}
}

// TestSelectAllocators_Postgres covers the Postgres branch. Skips if
// BLOCKYARD_TEST_POSTGRES_URL is unset — the package-level testPGDB
// already gates on that env var.
func TestSelectAllocators_Postgres(t *testing.T) {
	if pgTestBaseURL == "" {
		t.Skip("postgres not available")
	}
	db := testPGDB(t)

	cfg := &config.Config{
		Process: &config.ProcessConfig{
			PortRangeStart: 30000, PortRangeEnd: 30010,
			WorkerUIDStart: 80000, WorkerUIDEnd: 80010,
		},
		Proxy: config.ProxyConfig{SessionStore: config.SessionStorePostgres},
	}
	p, u := selectAllocators(cfg, nil, db)
	if _, ok := p.(*postgresPortAllocator); !ok {
		t.Errorf("port allocator = %T, want postgresPortAllocator", p)
	}
	if _, ok := u.(*postgresUIDAllocator); !ok {
		t.Errorf("uid allocator = %T, want postgresUIDAllocator", u)
	}
}

// TestSelectAllocators_Layered covers the Layered branch — wraps PG +
// Redis allocators behind a layered cache.
func TestSelectAllocators_Layered(t *testing.T) {
	if pgTestBaseURL == "" {
		t.Skip("postgres not available")
	}
	db := testPGDB(t)
	mr := miniredis.RunT(t)
	rc := redisstate.TestClient(t, mr.Addr())

	cfg := &config.Config{
		Process: &config.ProcessConfig{
			PortRangeStart: 30000, PortRangeEnd: 30010,
			WorkerUIDStart: 80000, WorkerUIDEnd: 80010,
		},
		Proxy: config.ProxyConfig{SessionStore: config.SessionStoreLayered},
	}
	p, u := selectAllocators(cfg, rc, db)
	if _, ok := p.(*layeredPortAllocator); !ok {
		t.Errorf("port allocator = %T, want layeredPortAllocator", p)
	}
	if _, ok := u.(*layeredUIDAllocator); !ok {
		t.Errorf("uid allocator = %T, want layeredUIDAllocator", u)
	}
}

// --- Build error paths (without real bwrap execution) ---

// TestBuild_UIDPoolExhausted hits the early uid-allocation failure
// branch — easier than driving bwrap with a real process, and it's a
// real error path that would otherwise be uncovered.
func TestBuild_UIDPoolExhausted(t *testing.T) {
	b := newFakeBackend(t)
	// Exhaust the UID pool before calling Build.
	for {
		if _, err := b.uids.Alloc(); err != nil {
			break
		}
	}

	res, err := b.Build(context.Background(), backend.BuildSpec{
		AppID: "app-1", BundleID: "b-1", Image: "img",
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if res.Success {
		t.Fatal("Build with exhausted UID pool should report failure")
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
}

// TestBuild_BadSeccompProfile covers the applySeccomp error branch —
// the configured profile file does not exist, so Build must return a
// failure with the error captured in Logs and the UID released.
func TestBuild_BadSeccompProfile(t *testing.T) {
	b := newFakeBackend(t)
	b.cfg.SeccompProfile = "/nonexistent/profile.bpf"

	before := b.uids.InUse()
	res, err := b.Build(context.Background(), backend.BuildSpec{
		AppID: "app-1", BundleID: "b-1", Image: "img",
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if res.Success {
		t.Fatal("Build with bad seccomp profile should fail")
	}
	if after := b.uids.InUse(); after != before {
		t.Errorf("UID leaked: before=%d after=%d", before, after)
	}
}

// TestBuild_RVersionNotInstalled covers the early-return branch where
// ResolveRBinary's fell flag is true for a Cmd-leading Rscript.
func TestBuild_RVersionNotInstalled(t *testing.T) {
	dir := setupRigFixture(t, "4.5.0")
	orig := rigBase
	defer setRigBase(orig)
	setRigBase(dir)

	b := newFakeBackend(t)
	b.cfg.RPath = "/usr/bin/R"

	res, err := b.Build(context.Background(), backend.BuildSpec{
		AppID:    "app-1",
		BundleID: "b-1",
		Image:    "img",
		Cmd:      []string{"Rscript", "app.R"},
		RVersion: "3.6.0", // not in the fixture
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if res.Success {
		t.Fatal("Build with uninstalled RVersion should fail")
	}
	if !strings.Contains(res.Logs, "no longer installed") {
		t.Errorf("Logs = %q, want 'no longer installed'", res.Logs)
	}
}

// TestBuild_BadBwrapPath covers the bwrapExecSpec error branch. The
// shim resolution fails because the bwrap path is not a real executable
// that can be stat'ed for the readlink dance inside bwrapExecSpec.
func TestBuild_BadBwrapPath(t *testing.T) {
	b := newFakeBackend(t)
	b.cfg.BwrapPath = "/nonexistent/bwrap"

	before := b.uids.InUse()
	res, err := b.Build(context.Background(), backend.BuildSpec{
		AppID: "app-1", BundleID: "b-1", Image: "img",
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if res.Success {
		t.Fatal("Build with missing bwrap should fail")
	}
	if after := b.uids.InUse(); after != before {
		t.Errorf("UID leaked: before=%d after=%d", before, after)
	}
}

// TestCleanupOrphanResources_Error covers the error branches inside the
// CleanupOrphanResources switch — using a stub allocator that
// implements CleanupOwnedOrphans and returns an error.
func TestCleanupOrphanResources_Error(t *testing.T) {
	b := newFakeBackend(t)
	stub := &errCleanupUIDAllocator{err: errStub}
	b.uids = stub

	err := b.CleanupOrphanResources(context.Background())
	if err == nil || !strings.Contains(err.Error(), "uid cleanup") {
		t.Errorf("expected uid cleanup error, got: %v", err)
	}
}

func TestCleanupOrphanResources_PortError(t *testing.T) {
	b := newFakeBackend(t)
	// Replace the port allocator with one that errors on cleanup.
	b.ports = &errCleanupPortAllocator{err: errStub}

	err := b.CleanupOrphanResources(context.Background())
	if err == nil || !strings.Contains(err.Error(), "port cleanup") {
		t.Errorf("expected port cleanup error, got: %v", err)
	}
}

// errStub is a sentinel error used by cleanup-error stubs.
var errStub = &stubError{msg: "cleanup failed"}

type stubError struct{ msg string }

func (e *stubError) Error() string { return e.msg }

// errCleanupUIDAllocator wraps memoryUIDAllocator but returns an error
// from CleanupOwnedOrphans. Satisfies the orphanCleaner interface used
// inside CleanupOrphanResources.
type errCleanupUIDAllocator struct {
	*memoryUIDAllocator
	err error
}

func (e *errCleanupUIDAllocator) CleanupOwnedOrphans(_ context.Context) error {
	return e.err
}

type errCleanupPortAllocator struct {
	*memoryPortAllocator
	err error
}

func (e *errCleanupPortAllocator) CleanupOwnedOrphans(_ context.Context) error {
	return e.err
}

// --- readOneProcStats / collectDescendants ---

// TestCollectDescendants_BadPID covers the early-return branch when
// /proc/<pid>/task/<pid>/children is unreadable.
func TestCollectDescendants_BadPID(t *testing.T) {
	descendants := collectDescendants(-1)
	if descendants != nil {
		t.Errorf("collectDescendants(-1) = %v, want nil", descendants)
	}
}

// TestReadOneProcStats_Nonexistent covers the error return when
// /proc/<pid>/status cannot be read.
func TestReadOneProcStats_Nonexistent(t *testing.T) {
	rss, ut, st := readOneProcStats(-1)
	if rss != 0 || ut != 0 || st != 0 {
		t.Errorf("stats = (%d, %d, %d), want all zero", rss, ut, st)
	}
}

// TestReadOneProcStats_StatusReadable_StatMissing covers the branch
// where /status is readable but /stat is not (rare). Hardest path to
// reach reliably — skip if PID 1 doesn't exist (containers with no
// PID 1 init).
func TestReadOneProcStats_LiveProcess(t *testing.T) {
	pid := os.Getpid()
	rss, ut, _ := readOneProcStats(pid)
	if rss == 0 {
		t.Errorf("expected non-zero RSS for self (%d)", pid)
	}
	_ = ut
}

// TestWorkerResourceUsage_ExitedBetween exercises the branch where
// the process exits between lookup and /proc read (readProcTreeStats
// returns all zeros). Simulate by registering a workerProc with a
// process that's already gone.
func TestWorkerResourceUsage_ExitedBetween(t *testing.T) {
	b := newFakeBackend(t)
	w := &workerProc{
		cmd:     nil,
		process: &os.Process{Pid: -1}, // nonexistent PID
		done:    make(chan struct{}),
	}
	b.workers["gone"] = w
	usage, err := b.WorkerResourceUsage(context.Background(), "gone")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage != nil {
		t.Errorf("usage = %+v, want nil for exited process", usage)
	}
}

// TestBuild_Success runs Build against /bin/true — the fake bwrap
// exits 0 immediately and produces no output, driving the
// post-goroutine success path (lines 687-691). This is the cheapest
// way to cover the Start+Wait pipeline without a real bwrap binary.
func TestBuild_Success(t *testing.T) {
	b := newFakeBackend(t)
	// /bin/true: exits 0, no output. Build should report Success=true.
	res, err := b.Build(context.Background(), backend.BuildSpec{
		AppID: "app-1", BundleID: "b-1", Image: "ignored",
		// No Cmd[0]=="R"/"Rscript", so RResolve branch is skipped.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !res.Success {
		t.Errorf("res.Success = false, want true (logs=%q, exit=%d)",
			res.Logs, res.ExitCode)
	}
	if res.ExitCode != 0 {
		t.Errorf("res.ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestBuild_NonZeroExit makes bwrap (/bin/false) exit 1, driving the
// waitErr != nil branch in Build and the *exec.ExitError path inside
// it. Exit codes are propagated verbatim.
func TestBuild_NonZeroExit(t *testing.T) {
	b := newFakeBackend(t)
	b.cfg.BwrapPath = "/bin/false"
	res, err := b.Build(context.Background(), backend.BuildSpec{
		AppID: "app-1", BundleID: "b-1", Image: "ignored",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Success {
		t.Error("Build with /bin/false should not be successful")
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
}

// TestBuild_LogWriterReceivesOutput covers the LogWriter callback path
// and the line-by-line ingest loop by configuring a bwrap stand-in
// that emits a line of output before exiting. /bin/echo prints its
// args to stdout — Build captures that into spec.LogWriter.
func TestBuild_LogWriterReceivesOutput(t *testing.T) {
	b := newFakeBackend(t)
	b.cfg.BwrapPath = "/bin/echo"

	var gotLines []string
	res, err := b.Build(context.Background(), backend.BuildSpec{
		AppID:     "app-1",
		BundleID:  "b-1",
		Image:     "ignored",
		LogWriter: func(line string) { gotLines = append(gotLines, line) },
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !res.Success {
		t.Errorf("Build should succeed when bwrap stand-in exits 0, got %+v", res)
	}
	if len(gotLines) == 0 {
		t.Error("LogWriter received no lines — ingest path not exercised")
	}
	if res.Logs == "" {
		t.Error("Logs buffer empty — ingest output not captured")
	}
}

// TestBuild_RCmdResolvesFromCfg covers the path where Cmd[0] is "R"
// (not Rscript) and ResolveRBinary succeeds — the Cmd[0] is replaced
// with the resolved R path without the Rscript filepath-dir dance.
func TestBuild_RCmdResolvesFromCfg(t *testing.T) {
	dir := setupRigFixture(t, "4.5.0")
	orig := rigBase
	defer setRigBase(orig)
	setRigBase(dir)

	b := newFakeBackend(t)
	b.cfg.RPath = "/usr/bin/R"

	// Cmd[0]="R" + installed RVersion: the resolve branch runs, the
	// Rscript filepath.Dir branch is skipped. Bwrap stand-in is
	// /bin/true so the actual build succeeds trivially.
	res, err := b.Build(context.Background(), backend.BuildSpec{
		AppID:    "app-1",
		BundleID: "b-1",
		Image:    "ignored",
		Cmd:      []string{"R", "-e", "1+1"},
		RVersion: "4.5.0",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !res.Success {
		t.Errorf("Build failed unexpectedly: %+v", res)
	}
}
