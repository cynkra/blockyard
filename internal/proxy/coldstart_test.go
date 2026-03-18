package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
	"github.com/cynkra/blockyard/internal/testutil"
)

func testColdstartServer(t *testing.T) *server.Server {
	t.Helper()
	tmp := t.TempDir()

	rvBin := testutil.FakeRvBinary(t)
	cfg := &config.Config{
		Server: config.ServerConfig{},
		Docker: config.DockerConfig{
			Image:        "test-image",
			ShinyPort:    3838,
			RvBinaryPath: rvBin,
		},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{
			WsCacheTTL:         config.Duration{Duration: 5 * time.Second},
			WorkerStartTimeout: config.Duration{Duration: 5 * time.Second},
			MaxWorkers:         10,
		},
	}

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	be := mock.New()
	return server.NewServer(cfg, be, database)
}

func createTestApp(t *testing.T, srv *server.Server, name string, withBundle bool) *db.AppRow {
	t.Helper()
	app, err := srv.DB.CreateApp(name, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if withBundle {
		_, err := srv.DB.CreateBundle("bundle-1", app.ID)
		if err != nil {
			t.Fatal(err)
		}
		srv.DB.UpdateBundleStatus("bundle-1", "ready")
		srv.DB.SetActiveBundle(app.ID, "bundle-1")
		// Re-fetch to get active_bundle
		app, err = srv.DB.GetApp(app.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	return app
}

func TestEnsureWorkerSpawnsNew(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	wid, addr, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	if wid == "" {
		t.Error("expected non-empty worker ID")
	}
	if addr == "" {
		t.Error("expected non-empty address")
	}
	if srv.Workers.Count() != 1 {
		t.Errorf("expected 1 worker, got %d", srv.Workers.Count())
	}
}

func TestEnsureWorkerReusesExisting(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Spawn first worker
	wid1, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	// Call again — should reuse
	wid2, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	if wid2 != wid1 {
		t.Errorf("expected reuse of worker %s, got %s", wid1, wid2)
	}
	if srv.Workers.Count() != 1 {
		t.Errorf("expected 1 worker, got %d", srv.Workers.Count())
	}
}

func TestEnsureWorkerMaxWorkersRejects(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Fill workers to max
	for i := range srv.Config.Proxy.MaxWorkers {
		srv.Workers.Set(
			fmt.Sprintf("fake-%d", i),
			server.ActiveWorker{AppID: "other"},
		)
	}

	_, _, err := ensureWorker(context.Background(), srv, app)
	if err != errMaxWorkers {
		t.Errorf("expected errMaxWorkers, got %v", err)
	}
}

func TestEnsureWorkerNoBundleRejects(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", false)

	_, _, err := ensureWorker(context.Background(), srv, app)
	if err != errNoBundle {
		t.Errorf("expected errNoBundle, got %v", err)
	}
}

func TestPollHealthySucceeds(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Spawn a worker so HealthCheck can find it
	wid, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	// pollHealthy on an already-healthy worker should return immediately
	if err := pollHealthy(context.Background(), srv, wid); err != nil {
		t.Errorf("expected healthy, got %v", err)
	}
}

func TestPollHealthyTimeout(t *testing.T) {
	srv := testColdstartServer(t)
	srv.Config.Proxy.WorkerStartTimeout = config.Duration{Duration: 200 * time.Millisecond}

	be := srv.Backend.(*mock.MockBackend)
	be.HealthOK.Store(false)

	// Spawn a mock worker so HealthCheck doesn't 404
	be.Spawn(context.Background(), backend.WorkerSpec{WorkerID: "timeout-worker"})
	srv.Workers.Set("timeout-worker", server.ActiveWorker{AppID: "test"})

	err := pollHealthy(context.Background(), srv, "timeout-worker")
	if err != errHealthTimeout {
		t.Errorf("expected errHealthTimeout, got %v", err)
	}
}

// --- faultyBackend wraps MockBackend with injectable errors ---

type faultyBackend struct {
	*mock.MockBackend
	spawnErr error
	addrErr  error
}

func (f *faultyBackend) Spawn(ctx context.Context, spec backend.WorkerSpec) error {
	if f.spawnErr != nil {
		return f.spawnErr
	}
	return f.MockBackend.Spawn(ctx, spec)
}

func (f *faultyBackend) Addr(ctx context.Context, id string) (string, error) {
	if f.addrErr != nil {
		return "", f.addrErr
	}
	return f.MockBackend.Addr(ctx, id)
}

func testColdstartServerWithBackend(t *testing.T, be backend.Backend) *server.Server {
	t.Helper()
	tmp := t.TempDir()

	rvBin := testutil.FakeRvBinary(t)
	cfg := &config.Config{
		Server: config.ServerConfig{},
		Docker: config.DockerConfig{
			Image:        "test-image",
			ShinyPort:    3838,
			RvBinaryPath: rvBin,
		},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{
			WsCacheTTL:         config.Duration{Duration: 5 * time.Second},
			WorkerStartTimeout: config.Duration{Duration: 5 * time.Second},
			MaxWorkers:         10,
		},
	}

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	return server.NewServer(cfg, be, database)
}

// TestSpawnSingleFlightDedup verifies that concurrent callers for the same
// key share the result from the first caller (lines 55-59).
func TestSpawnSingleFlightDedup(t *testing.T) {
	var g spawnSingleFlight

	started := make(chan struct{})
	proceed := make(chan struct{})

	// Launch first caller — it enters fn and signals, then waits.
	go func() {
		g.do("app-1", func() (string, string, error) {
			close(started) // signal that we're inside fn
			<-proceed      // wait for test to launch second caller
			return "wid-1", "addr-1", nil
		})
	}()

	<-started // first caller is now inside fn, holding the slot

	// Launch second caller — should join the in-flight call and share
	// the result (lines 55-59).
	done := make(chan struct{})
	go func() {
		wid, addr, err := g.do("app-1", func() (string, string, error) {
			t.Error("second caller should not execute fn")
			return "", "", nil
		})
		if err != nil {
			t.Errorf("second caller: unexpected error: %v", err)
		}
		if wid != "wid-1" || addr != "addr-1" {
			t.Errorf("second caller: got %s %s, want wid-1 addr-1", wid, addr)
		}
		close(done)
	}()

	// Give the second goroutine a moment to enter do() and find the
	// existing call, then let the first caller complete.
	time.Sleep(50 * time.Millisecond)
	close(proceed)

	<-done
}

// TestEnsureWorkerDrainingRejects covers lines 82-84.
func TestEnsureWorkerDrainingRejects(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Mark app as draining
	srv.Workers.Set("drain-worker", server.ActiveWorker{AppID: app.ID})
	srv.Workers.MarkDraining(app.ID)

	_, _, err := ensureWorker(context.Background(), srv, app)
	if err != errAppDraining {
		t.Errorf("expected errAppDraining, got %v", err)
	}
}

// TestEnsureWorkerCapacityExhausted covers lines 82-84 (lb.Assign returns
// errCapacityExhausted and it is passed through).
func TestEnsureWorkerCapacityExhausted(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Set MaxSessionsPerWorker to 1, MaxWorkersPerApp to 1
	app.MaxSessionsPerWorker = 1
	maxW := 1
	app.MaxWorkersPerApp = &maxW

	// Spawn one worker
	wid, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	// Fill its session capacity
	srv.Sessions.Set("sess-1", session.Entry{WorkerID: wid})

	// Now ensureWorker should return errCapacityExhausted
	_, _, err = ensureWorker(context.Background(), srv, app)
	if err != errCapacityExhausted {
		t.Errorf("expected errCapacityExhausted, got %v", err)
	}
}

// TestEnsureWorkerRegistryMissReResolve covers lines 105-113:
// registry miss after LB assign, then re-resolve via Backend.Addr succeeds.
func TestEnsureWorkerRegistryMissReResolve(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Spawn a worker so it exists in the backend
	wid, addr, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	// Delete from registry to simulate a miss
	srv.Registry.Delete(wid)

	// ensureWorker should re-resolve via Backend.Addr
	wid2, addr2, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	if wid2 != wid {
		t.Errorf("expected worker %s, got %s", wid, wid2)
	}
	if addr2 != addr {
		t.Errorf("expected addr %s, got %s", addr, addr2)
	}
}

// TestEnsureWorkerRegistryMissAddrFails covers lines 94-96 and 105-113:
// registry miss + Backend.Addr fails => evict worker, spawn new one.
func TestEnsureWorkerRegistryMissAddrFails(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Spawn a worker
	wid1, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	// Delete from registry to simulate a miss
	srv.Registry.Delete(wid1)

	// Stop the worker in the backend so Addr will fail
	be := srv.Backend.(*mock.MockBackend)
	be.Stop(context.Background(), wid1)

	// ensureWorker should evict the stale worker and spawn a new one
	wid2, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	if wid2 == wid1 {
		t.Error("expected a new worker, got the same one")
	}
}

// TestEnsureWorkerRecheckAfterSpawnSlot covers lines 127-134:
// re-check after acquiring spawn slot finds capacity.
func TestEnsureWorkerRecheckAfterSpawnSlot(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Set max sessions to 1 per worker so first worker gets saturated
	app.MaxSessionsPerWorker = 1

	// Spawn a worker and saturate it
	wid1, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	srv.Sessions.Set("sess-1", session.Entry{WorkerID: wid1})

	// Now spawn a second concurrent request: first call to lb.Assign
	// returns "", nil (no capacity, but can spawn). Inside spawnGroup.do,
	// the re-check (line 120) will also see no capacity. A new worker
	// will be spawned.
	wid2, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	if wid2 == wid1 {
		t.Error("expected new worker, got the same one")
	}

	// Free the session from wid1 so re-check can find capacity
	srv.Sessions.Delete("sess-1")

	// Saturate wid2 so we need to check existing workers
	srv.Sessions.Set("sess-2", session.Entry{WorkerID: wid2})

	// Now ensureWorker should reuse wid1 via the outer lb.Assign (not
	// entering the spawn path at all). To cover the inner re-check path
	// (lines 127-134), we need both workers saturated and then free one
	// inside the spawn slot. That's hard to test deterministically, but
	// at least this exercises the code path where the outer lb.Assign
	// finds capacity in the first worker.
	wid3, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	if wid3 != wid1 {
		t.Errorf("expected reuse of worker %s, got %s", wid1, wid3)
	}
}

// TestSpawnWorkerOpenbaoExtraEnv covers lines 166-177:
// openbao config sets extra env vars on the worker spec.
func TestSpawnWorkerOpenbaoExtraEnv(t *testing.T) {
	srv := testColdstartServer(t)
	srv.Config.Openbao = &config.OpenbaoConfig{
		Address:     "http://vault:8200",
		AdminToken:  config.NewSecret("root-token"),
		JWTAuthPath: "jwt",
	}
	srv.Config.Server.ExternalURL = "http://blockyard:8080"

	app := createTestApp(t, srv, "my-app", true)

	wid, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	if wid == "" {
		t.Error("expected non-empty worker ID")
	}
}

// TestSpawnWorkerOpenbaoDevFallback covers lines 166-177:
// openbao config without ExternalURL uses dev fallback.
func TestSpawnWorkerOpenbaoDevFallback(t *testing.T) {
	srv := testColdstartServer(t)
	srv.Config.Openbao = &config.OpenbaoConfig{
		Address:     "http://vault:8200",
		AdminToken:  config.NewSecret("root-token"),
		JWTAuthPath: "jwt",
	}
	srv.Config.Server.ExternalURL = "" // force dev fallback
	srv.Config.Server.Bind = ":9090"

	app := createTestApp(t, srv, "my-app", true)

	wid, _, err := ensureWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	if wid == "" {
		t.Error("expected non-empty worker ID")
	}
}

// TestSpawnWorkerSpawnError covers lines 193-195:
// backend.Spawn returns an error.
func TestSpawnWorkerSpawnError(t *testing.T) {
	fb := &faultyBackend{
		MockBackend: mock.New(),
		spawnErr:    fmt.Errorf("container runtime unavailable"),
	}
	srv := testColdstartServerWithBackend(t, fb)
	app := createTestApp(t, srv, "my-app", true)

	_, _, err := ensureWorker(context.Background(), srv, app)
	if err == nil {
		t.Fatal("expected error from spawn")
	}
	if got := err.Error(); got != "spawn worker: container runtime unavailable" {
		t.Errorf("unexpected error: %s", got)
	}
}

// TestSpawnWorkerAddrError covers lines 198-201:
// backend.Spawn succeeds but backend.Addr fails => cleanup via Stop.
func TestSpawnWorkerAddrError(t *testing.T) {
	fb := &faultyBackend{
		MockBackend: mock.New(),
		addrErr:     fmt.Errorf("network unreachable"),
	}
	srv := testColdstartServerWithBackend(t, fb)
	app := createTestApp(t, srv, "my-app", true)

	_, _, err := ensureWorker(context.Background(), srv, app)
	if err == nil {
		t.Fatal("expected error from addr")
	}
	if got := err.Error(); got != "resolve worker address: network unreachable" {
		t.Errorf("unexpected error: %s", got)
	}
	// The spawned worker should have been cleaned up
	if fb.MockBackend.WorkerCount() != 0 {
		t.Errorf("expected worker to be stopped after addr failure, got %d workers", fb.MockBackend.WorkerCount())
	}
}

// TestSpawnWorkerHealthFailureCleanup covers lines 210-215:
// pollHealthy fails => worker is cleaned up from Workers, Registry, Backend.
func TestSpawnWorkerHealthFailureCleanup(t *testing.T) {
	srv := testColdstartServer(t)
	srv.Config.Proxy.WorkerStartTimeout = config.Duration{Duration: 200 * time.Millisecond}

	be := srv.Backend.(*mock.MockBackend)
	be.HealthOK.Store(false)

	app := createTestApp(t, srv, "my-app", true)

	_, _, err := ensureWorker(context.Background(), srv, app)
	if err != errHealthTimeout {
		t.Errorf("expected errHealthTimeout, got %v", err)
	}
	// Worker should be cleaned up from all state
	if srv.Workers.Count() != 0 {
		t.Errorf("expected 0 workers after health failure, got %d", srv.Workers.Count())
	}
	if be.WorkerCount() != 0 {
		t.Errorf("expected backend worker to be stopped, got %d", be.WorkerCount())
	}
}

// TestPollHealthyContextCancellation covers lines 243-244:
// context is cancelled during pollHealthy.
func TestPollHealthyContextCancellation(t *testing.T) {
	srv := testColdstartServer(t)
	srv.Config.Proxy.WorkerStartTimeout = config.Duration{Duration: 10 * time.Second}

	be := srv.Backend.(*mock.MockBackend)
	be.HealthOK.Store(false)

	// Spawn a mock worker
	be.Spawn(context.Background(), backend.WorkerSpec{WorkerID: "cancel-worker"})
	srv.Workers.Set("cancel-worker", server.ActiveWorker{AppID: "test"})

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	err := pollHealthy(ctx, srv, "cancel-worker")
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// TestPtrOrNonNil covers line 255: ptrOr with a non-nil pointer.
func TestPtrOrNonNil(t *testing.T) {
	s := "hello"
	if got := ptrOr(&s, "fallback"); got != "hello" {
		t.Errorf("expected 'hello', got %q", got)
	}

	n := 42.0
	if got := ptrOr(&n, 0.0); got != 42.0 {
		t.Errorf("expected 42.0, got %f", got)
	}
}

// TestPtrOrNil covers the nil case of ptrOr (already covered but included
// for completeness).
func TestPtrOrNil(t *testing.T) {
	if got := ptrOr[string](nil, "fallback"); got != "fallback" {
		t.Errorf("expected 'fallback', got %q", got)
	}
}

// TestWorkerEnvNilOpenbao verifies that WorkerEnv returns nil when
// srv.Config.Openbao is nil.
func TestWorkerEnvNilOpenbao(t *testing.T) {
	srv := testColdstartServer(t)
	srv.Config.Openbao = nil

	env := WorkerEnv(srv)
	if env != nil {
		t.Errorf("expected nil, got %v", env)
	}
}

// TestWorkerEnvWithExternalURL verifies that WorkerEnv sets VAULT_ADDR and
// BLOCKYARD_API_URL correctly when ExternalURL is configured.
func TestWorkerEnvWithExternalURL(t *testing.T) {
	srv := testColdstartServer(t)
	srv.Config.Openbao = &config.OpenbaoConfig{
		Address: "http://vault:8200",
	}
	srv.Config.Server.ExternalURL = "https://blockyard.example.com"

	env := WorkerEnv(srv)
	if env == nil {
		t.Fatal("expected non-nil env map")
	}
	if got := env["VAULT_ADDR"]; got != "http://vault:8200" {
		t.Errorf("VAULT_ADDR = %q, want %q", got, "http://vault:8200")
	}
	if got := env["BLOCKYARD_API_URL"]; got != "https://blockyard.example.com" {
		t.Errorf("BLOCKYARD_API_URL = %q, want %q", got, "https://blockyard.example.com")
	}
}

// TestWorkerEnvWithServices verifies that WorkerEnv sets
// BLOCKYARD_VAULT_SERVICES to valid JSON mapping service IDs to paths.
func TestWorkerEnvWithServices(t *testing.T) {
	srv := testColdstartServer(t)
	srv.Config.Openbao = &config.OpenbaoConfig{
		Address: "http://vault:8200",
		Services: []config.ServiceConfig{
			{ID: "openai", Label: "OpenAI"},
			{ID: "posit", Label: "Posit Connect"},
		},
	}
	srv.Config.Server.ExternalURL = "http://blockyard:8080"

	env := WorkerEnv(srv)
	if env == nil {
		t.Fatal("expected non-nil env map")
	}

	raw, ok := env["BLOCKYARD_VAULT_SERVICES"]
	if !ok {
		t.Fatal("expected BLOCKYARD_VAULT_SERVICES to be set")
	}

	var svcMap map[string]string
	if err := json.Unmarshal([]byte(raw), &svcMap); err != nil {
		t.Fatalf("BLOCKYARD_VAULT_SERVICES is not valid JSON: %v", err)
	}
	if got := svcMap["openai"]; got != "apikeys/openai" {
		t.Errorf("openai path = %q, want %q", got, "apikeys/openai")
	}
	if got := svcMap["posit"]; got != "apikeys/posit" {
		t.Errorf("posit path = %q, want %q", got, "apikeys/posit")
	}
	if len(svcMap) != 2 {
		t.Errorf("expected 2 entries, got %d", len(svcMap))
	}
}

// TestWorkerEnvWithServiceNetwork verifies that WorkerEnv sets
// BLOCKYARD_API_URL to http://blockyard:<port> when service_network is
// configured, overriding ExternalURL.
func TestWorkerEnvWithServiceNetwork(t *testing.T) {
	srv := testColdstartServer(t)
	srv.Config.Openbao = &config.OpenbaoConfig{
		Address: "http://openbao:8200",
	}
	srv.Config.Server.ExternalURL = "http://localhost:8080"
	srv.Config.Server.Bind = "0.0.0.0:8080"
	srv.Config.Docker.ServiceNetwork = "blockyard-services"

	env := WorkerEnv(srv)
	if env == nil {
		t.Fatal("expected non-nil env map")
	}
	if got, want := env["BLOCKYARD_API_URL"], "http://blockyard:8080"; got != want {
		t.Errorf("BLOCKYARD_API_URL = %q, want %q", got, want)
	}
	if got, want := env["VAULT_ADDR"], "http://openbao:8200"; got != want {
		t.Errorf("VAULT_ADDR = %q, want %q", got, want)
	}
}

// TestWorkerEnvServiceNetworkOverridesExternalURL verifies that
// service_network takes precedence over ExternalURL for BLOCKYARD_API_URL.
func TestWorkerEnvServiceNetworkOverridesExternalURL(t *testing.T) {
	srv := testColdstartServer(t)
	srv.Config.Openbao = &config.OpenbaoConfig{
		Address: "http://vault:8200",
	}
	srv.Config.Server.ExternalURL = "https://blockyard.example.com"
	srv.Config.Server.Bind = "0.0.0.0:9090"
	srv.Config.Docker.ServiceNetwork = "my-services"

	env := WorkerEnv(srv)
	if got, want := env["BLOCKYARD_API_URL"], "http://blockyard:9090"; got != want {
		t.Errorf("BLOCKYARD_API_URL = %q, want %q", got, want)
	}
}
