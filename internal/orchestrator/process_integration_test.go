//go:build process_test

package orchestrator_test

// End-to-end rolling-update test for the process orchestrator.
//
// The test builds the current source tree into a real blockyard
// binary (TestMain), pins executableFn to it via the export_test
// seam, then exercises the load-bearing cutover path in two layers:
//
//   TestProcessFactoryCreatesAndActivates — directly calls
//     processServerFactory.CreateInstance to fork+exec a real
//     blockyard child with BLOCKYARD_PASSIVE=1, polls /readyz, POSTs
//     /admin/activate with the activation token, verifies the
//     passive→active transition, and tears the child down via
//     inst.Kill(ctx). Exercises the env-var propagation (including
//     the applyEnvOverrides reflective walker for
//     BLOCKYARD_SERVER_BIND), the --config flag passthrough via
//     Config.ConfigPath, and the kill/reap path.
//
//   TestProcessOrchestratorFullUpdate — wires a real Orchestrator
//     with the process factory and a mock update checker, runs the
//     full Update(ctx) → waitReady → drain → activate → Watchdog
//     flow against a live child, and verifies the state machine
//     and drain/undrain closures fire in the expected order.
//
// Skipped when bwrap is unavailable (matches the rest of the
// process_test matrix).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/orchestrator"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/update"
)

var builtBlockyardBinary string

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		// Integration test requires bwrap for the child blockyard's
		// preflight probe binary. The rest of the process_test
		// matrix skips the same way.
		os.Exit(0)
	}
	// Build the current source tree into a scratch binary so
	// CreateInstance re-execs the right thing. This runs once per
	// test binary invocation.
	tmp, err := os.MkdirTemp("", "blockyard-orch-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "process_integration_test: mkdir temp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	builtBlockyardBinary = filepath.Join(tmp, "blockyard")
	cmd := exec.Command("go", "build", "-o", builtBlockyardBinary,
		"github.com/cynkra/blockyard/cmd/blockyard")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "process_integration_test: build blockyard:", err)
		os.Exit(1)
	}
	orchestrator.ExecutableFnForTest(func() (string, error) {
		return builtBlockyardBinary, nil
	})
	os.Exit(m.Run())
}

// writeChildConfig prepares a minimal blockyard.toml + tmp dirs for
// a child blockyard and returns the loaded *config.Config with
// ConfigPath already populated.
//
// Uses skip_preflight=true so the child doesn't need R installed, a
// working egress firewall, or host-effective bwrap UID mapping —
// those are matrix-specific concerns covered by the regular
// process_test jobs. This test cares about the orchestrator/factory
// path, not the bwrap host UID check.
func writeChildConfig(t *testing.T, mr *miniredis.Miniredis, primaryPort int, altRange string) *config.Config {
	t.Helper()
	tmp := t.TempDir()

	bundleDir := filepath.Join(tmp, "bundles")
	dbDir := filepath.Join(tmp, "db")
	for _, d := range []string{bundleDir, dbDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The process backend's ensureBundleMountPoint needs the
	// worker bundle path to exist on the host. A /tmp-rooted path
	// is fine.
	workerMount, err := os.MkdirTemp("", "blockyard-e2e-app-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(workerMount) })

	cfgText := fmt.Sprintf(`
[server]
bind = "127.0.0.1:%d"
backend = "process"
skip_preflight = true
shutdown_timeout = "5s"
drain_timeout = "5s"

[storage]
bundle_server_path = "%s"
bundle_worker_path = "%s"

[database]
driver = "sqlite"
path = "%s/blockyard.db"

[process]
bwrap_path = "/usr/bin/bwrap"
r_path = "/bin/true"
port_range_start = 22000
port_range_end = 22099
worker_uid_range_start = 70000
worker_uid_range_end = 70099
worker_gid = 65534

[redis]
url = "redis://%s"

[update]
alt_bind_range = "%s"
drain_idle_wait = "1s"
watch_period = "500ms"

[proxy]
worker_start_timeout = "30s"
`, primaryPort, bundleDir, workerMount, dbDir, mr.Addr(), altRange)

	cfgPath := filepath.Join(tmp, "blockyard.toml")
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.ConfigPath = cfgPath
	return cfg
}

// TestProcessFactoryCreatesAndActivates exercises the factory's
// CreateInstance path end-to-end against a real child blockyard.
// This is the minimum-scope E2E that verifies fork+exec, env-var
// propagation, the passive-mode HTTP server startup, the activation
// endpoint round-trip, and Kill.
func TestProcessFactoryCreatesAndActivates(t *testing.T) {
	mr := miniredis.RunT(t)

	// Primary port is unused (no "old" server runs on it in this
	// test) but must be a known host:port so pickAltPort can probe
	// on the primary host. 127.0.0.1:<random> works.
	primaryPort := findFreePort(t)
	altStart := findFreePort(t)
	altRange := fmt.Sprintf("%d-%d", altStart, altStart+4)

	cfg := writeChildConfig(t, mr, primaryPort, altRange)
	factory := orchestrator.NewProcessFactory(cfg, "1.0.0-test")

	sender := task.NewStore().Create("factory-test", "test")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const token = "test-activation-token"
	inst, err := factory.CreateInstance(ctx, "", []string{
		"BLOCKYARD_ACTIVATION_TOKEN=" + token,
	}, sender)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	// Kill the child before miniredis goes away. t.Cleanup runs
	// in LIFO order, so this kill runs before miniredis's own
	// cleanup (registered earlier via miniredis.RunT).
	t.Cleanup(func() {
		killCtx, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer killCancel()
		inst.Kill(killCtx)
	})

	if inst.ID() == "" {
		t.Error("instance ID should not be empty")
	}
	if inst.Addr() == "" {
		t.Error("instance Addr should not be empty")
	}
	t.Logf("child blockyard spawned: id=%s addr=%s", inst.ID(), inst.Addr())

	// Wait for /readyz=200 — the child must boot, load config, open
	// the DB, ping miniredis, start the HTTP server, and return 200
	// on /readyz despite being in passive mode.
	if err := waitHTTP(ctx, "http://"+inst.Addr()+"/readyz", http.StatusOK); err != nil {
		t.Fatalf("child /readyz never returned 200: %v", err)
	}

	// /readyz body should indicate passive mode (since the caller
	// is unauthenticated, details are suppressed, but the "mode"
	// field is public).
	body := httpBody(t, "http://"+inst.Addr()+"/readyz")
	if !strings.Contains(body, `"mode":"passive"`) {
		t.Errorf("expected passive mode in /readyz body, got: %s", body)
	}

	// POST /api/v1/admin/activate with the activation token — this
	// is the same call the orchestrator's activate() helper makes.
	activateURL := "http://" + inst.Addr() + "/api/v1/admin/activate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, activateURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /admin/activate: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("activate returned %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// After activation, /readyz should no longer report passive mode.
	// Retry briefly since the passive flag flip happens on another
	// goroutine.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b := httpBody(t, "http://"+inst.Addr()+"/readyz")
		if !strings.Contains(b, `"mode":"passive"`) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	body = httpBody(t, "http://"+inst.Addr()+"/readyz")
	if strings.Contains(body, `"mode":"passive"`) {
		t.Errorf("expected active mode after activation, still passive: %s", body)
	}
}

// TestProcessOrchestratorFullUpdate wires a real Orchestrator with
// the process factory and a mock update checker, then runs the full
// Update + Watchdog flow against a live child blockyard. Exercises:
//
//   - orchestrator.Update's sequencing (preupdate → backup →
//     CreateInstance → waitReady → drain → activate → stash on
//     activeInstance)
//   - the drain/undrain closures firing in the expected order
//   - Watchdog reading activeInstance.Addr() and polling /readyz
//   - the state machine transitions (idle → updating → watching → idle)
//
// Does NOT exercise worker spawning or the FinishIdleWait session
// count — those require a real R binary and bwrap UID mapping, both
// covered separately by the process_test matrix.
func TestProcessOrchestratorFullUpdate(t *testing.T) {
	mr := miniredis.RunT(t)

	primaryPort := findFreePort(t)
	altStart := findFreePort(t)
	altRange := fmt.Sprintf("%d-%d", altStart, altStart+4)

	cfg := writeChildConfig(t, mr, primaryPort, altRange)
	factory := orchestrator.NewProcessFactory(cfg, "1.0.0")

	database, err := db.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	checker := &e2eChecker{result: &update.Result{
		CurrentVersion:  "1.0.0",
		LatestVersion:   "2.0.0",
		UpdateAvailable: true,
	}}

	var drainCalls, undrainCalls, exitCalls atomic.Int32
	orch := orchestrator.New(
		factory, database, cfg, "1.0.0",
		task.NewStore(), checker, slog.Default(),
		func() { drainCalls.Add(1) },
		func() { undrainCalls.Add(1) },
		func() { exitCalls.Add(1) },
	)

	sender := task.NewStore().Create("full-update-test", "test")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Kill any leaked child before miniredis teardown. Registered
	// before Update runs so it fires in the right order regardless
	// of where the test errors out. orch.ActiveInstanceForTest reads
	// the field directly.
	t.Cleanup(func() {
		if inst := orchestrator.ActiveInstanceForTest(orch); inst != nil {
			killCtx, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer killCancel()
			inst.Kill(killCtx)
		}
	})

	updated, err := orch.Update(ctx, "stable", sender)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !updated {
		t.Fatal("expected updated=true from Update")
	}
	if got := drainCalls.Load(); got != 1 {
		t.Errorf("drainFn called %d times, want 1", got)
	}
	if got := undrainCalls.Load(); got != 0 {
		t.Errorf("undrainFn called %d times, want 0 (happy path)", got)
	}

	// activeInstance should be set and its addr in the alt range.
	inst := orchestrator.ActiveInstanceForTest(orch)
	if inst == nil {
		t.Fatal("activeInstance should be set after Update")
	}
	_, port, _ := net.SplitHostPort(inst.Addr())
	t.Logf("active instance: id=%s addr=%s (port=%s)", inst.ID(), inst.Addr(), port)

	// Verify the child is alive and serving.
	if err := waitHTTP(ctx, "http://"+inst.Addr()+"/healthz", http.StatusOK); err != nil {
		t.Fatalf("child /healthz never returned 200 after Update: %v", err)
	}

	// Run Watchdog with a short period — the child should stay
	// healthy and Watchdog should return nil.
	watchCtx, watchCancel := context.WithTimeout(ctx, 10*time.Second)
	defer watchCancel()
	if err := orch.Watchdog(watchCtx, 500*time.Millisecond, sender); err != nil {
		t.Errorf("Watchdog on healthy child: %v", err)
	}

	// After Watchdog clears activeInstance, but we retained `inst`
	// above, so the cleanup closure can still kill it. The child
	// is intentionally alive here — in production the old server
	// would now exit (via exitFn from the scheduled path) and the
	// child would continue serving.
	if got := exitCalls.Load(); got != 0 {
		t.Errorf("exitFn called %d times — test drives Watchdog directly, not scheduled", got)
	}

	// State machine should be back to idle.
	if s := orch.State(); s != "idle" {
		t.Errorf("state after Watchdog = %q, want idle", s)
	}

	// Manual kill via the retained handle so the child goes away
	// before miniredis.
	killCtx, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer killCancel()
	inst.Kill(killCtx)
	// Wait for the process to actually go away — Kill returns
	// asynchronously but we want the test to finish cleanly.
	time.Sleep(200 * time.Millisecond)
}

// e2eChecker is a test-only update checker that returns a fixed result.
type e2eChecker struct {
	result *update.Result
	err    error
}

func (c *e2eChecker) CheckLatest(_, _ string) (*update.Result, error) {
	return c.result, c.err
}

// findFreePort asks the kernel for an ephemeral port and immediately
// closes the listener. The returned port may be reclaimed before the
// caller binds it, but for the orchestrator test we use the port only
// as a base for the alt range or as a never-bound primary placeholder.
func findFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// waitHTTP blocks until the given URL returns the expected status
// or ctx is cancelled / the deadline elapses. Poll interval is
// 200ms — long enough to not hammer the child, short enough that
// the test reports the first healthy response within ~200ms.
func waitHTTP(ctx context.Context, url string, want int) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:gosec // test helper
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == want {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for %s to return %d", url, want)
}

// httpBody reads and returns the body of a GET. Test helper only —
// bails via t.Fatal on error.
func httpBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // test helper
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// ensureNotRunning verifies that no process with the given pid is
// alive. Used by cleanup validation. Unused placeholder, kept so
// future sub-tests can verify process teardown.
func ensureNotRunning(t *testing.T, pid int) { //nolint:unused
	t.Helper()
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := p.Signal(syscall.Signal(0)); err == nil {
		t.Errorf("process %d still running", pid)
	}
}
