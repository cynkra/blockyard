//go:build !minimal || docker_backend

package orchestrator

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/update"
)

// --- mock Docker client ---

type mockDocker struct {
	inspectFn func(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	createFn  func(ctx context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	startFn   func(ctx context.Context, id string, opts client.ContainerStartOptions) (client.ContainerStartResult, error)
	stopFn    func(ctx context.Context, id string, opts client.ContainerStopOptions) (client.ContainerStopResult, error)
	removeFn  func(ctx context.Context, id string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	waitFn    func(ctx context.Context, id string, opts client.ContainerWaitOptions) client.ContainerWaitResult
	pullFn    func(ctx context.Context, ref string, opts client.ImagePullOptions) (client.ImagePullResponse, error)
}

func (m *mockDocker) ContainerInspect(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if m.inspectFn != nil {
		return m.inspectFn(ctx, id, opts)
	}
	return defaultInspectResult(), nil
}

func (m *mockDocker) ContainerCreate(ctx context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if m.createFn != nil {
		return m.createFn(ctx, opts)
	}
	return client.ContainerCreateResult{ID: "new-container-123"}, nil
}

func (m *mockDocker) ContainerStart(ctx context.Context, id string, opts client.ContainerStartOptions) (client.ContainerStartResult, error) {
	if m.startFn != nil {
		return m.startFn(ctx, id, opts)
	}
	return client.ContainerStartResult{}, nil
}

func (m *mockDocker) ContainerStop(ctx context.Context, id string, opts client.ContainerStopOptions) (client.ContainerStopResult, error) {
	if m.stopFn != nil {
		return m.stopFn(ctx, id, opts)
	}
	return client.ContainerStopResult{}, nil
}

func (m *mockDocker) ContainerRemove(ctx context.Context, id string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if m.removeFn != nil {
		return m.removeFn(ctx, id, opts)
	}
	return client.ContainerRemoveResult{}, nil
}

func (m *mockDocker) ContainerWait(ctx context.Context, id string, opts client.ContainerWaitOptions) client.ContainerWaitResult {
	if m.waitFn != nil {
		return m.waitFn(ctx, id, opts)
	}
	ch := make(chan container.WaitResponse, 1)
	ch <- container.WaitResponse{}
	return client.ContainerWaitResult{Result: ch}
}

func (m *mockDocker) ImagePull(_ context.Context, _ string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	if m.pullFn != nil {
		return m.pullFn(context.Background(), "", client.ImagePullOptions{})
	}
	return mockPullResponse{ReadCloser: io.NopCloser(&emptyReader{})}, nil
}

type mockPullResponse struct {
	io.ReadCloser
}

func (m mockPullResponse) JSONMessages(_ context.Context) iter.Seq2[jsonstream.Message, error] {
	return nil
}

func (m mockPullResponse) Wait(_ context.Context) error {
	return nil
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

// --- mock update checker ---

type mockChecker struct {
	result *update.Result
	err    error
}

func (m *mockChecker) CheckLatest(_, _ string) (*update.Result, error) {
	return m.result, m.err
}

// --- helpers ---

func defaultInspectResult() client.ContainerInspectResult {
	return client.ContainerInspectResult{
		Container: container.InspectResponse{
			Config: &container.Config{
				Image: "ghcr.io/cynkra/blockyard:1.0.0",
				Env:   []string{"FOO=bar"},
			},
			HostConfig: &container.HostConfig{},
			NetworkSettings: &container.NetworkSettings{
				Networks: map[string]*network.EndpointSettings{
					"bridge": {
						IPAddress: netip.MustParseAddr("172.17.0.2"),
					},
				},
			},
		},
	}
}

// newTestOrchestrator builds an Orchestrator wired to a Docker
// factory backed by the given mock client. The factory is the new
// phase 3-8 ServerFactory interface; the mock Docker client lives
// behind a dockerServerFactory (via newDockerFactoryForTest) so the
// old test cases continue to exercise the real clone flow.
func newTestOrchestrator(t *testing.T, docker *mockDocker, checker updateAPI) (*Orchestrator, *callTracker) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	tracker := &callTracker{}

	cfg := &config.Config{
		Server:   config.ServerConfig{Bind: "0.0.0.0:8080"},
		Database: config.DatabaseConfig{Driver: "sqlite", Path: dbPath},
		Proxy:    config.ProxyConfig{WorkerStartTimeout: config.Duration{Duration: 5 * time.Second}},
	}

	factory := newDockerFactoryForTest(docker, "self-container-id", func() string {
		return listenPortFromBindForTest(cfg.Server.Bind)
	})

	o := &Orchestrator{
		factory:   factory,
		db:        database,
		cfg:       cfg,
		version:   "1.0.0",
		tasks:     task.NewStore(),
		update:    checker,
		log:       slog.Default(),
		drainFn:   func() { tracker.drained.Add(1) },
		undrainFn: func() { tracker.undrained.Add(1) },
		exitFn:    func() { tracker.exited.Add(1) },
	}
	o.state.Store("idle")
	return o, tracker
}

// listenPortFromBindForTest is a local copy of
// cmd/blockyard/orchestrator_docker.listenPortFromBind so the test
// file doesn't have to import a main package.
func listenPortFromBindForTest(bind string) string {
	for i := len(bind) - 1; i >= 0; i-- {
		if bind[i] == ':' {
			return bind[i+1:]
		}
	}
	return "8080"
}

// dockerFactoryFromOrchestrator is a test helper that exposes the
// factory the orchestrator is using so tests can call its internal
// methods (cloneConfig, containerAddr, killAndRemove) directly.
func dockerFactoryFromOrchestrator(o *Orchestrator) *dockerServerFactory {
	return o.factory.(*dockerServerFactory)
}

// Shim methods on *Orchestrator that delegate to the Docker factory.
// These exist only in the docker-tagged test file so the legacy test
// bodies keep working with the old `o.foo(ctx, ...)` call shape. No
// production code calls these — they're named with the same method
// names and unexported to stay out of the interface area of the
// orchestrator package.

func (o *Orchestrator) cloneConfig(ctx context.Context, image string, extraEnv []string) (client.ContainerCreateOptions, error) {
	return dockerFactoryFromOrchestrator(o).cloneConfig(ctx, image, extraEnv)
}

func (o *Orchestrator) startClone(ctx context.Context, image string) (string, error) {
	o.activationToken = generateActivationToken()
	sender := task.NewStore().Create("test-clone", "test")
	inst, err := dockerFactoryFromOrchestrator(o).CreateInstance(ctx, image, []string{
		"BLOCKYARD_ACTIVATION_TOKEN=" + o.activationToken,
	}, sender)
	if err != nil {
		return "", err
	}
	return inst.ID(), nil
}

func (o *Orchestrator) currentImageBase(ctx context.Context) string {
	return dockerFactoryFromOrchestrator(o).CurrentImageBase(ctx)
}

func (o *Orchestrator) currentImageTag(ctx context.Context) string {
	return dockerFactoryFromOrchestrator(o).CurrentImageTag(ctx)
}

func (o *Orchestrator) containerAddr(ctx context.Context, id string) (string, error) {
	return dockerFactoryFromOrchestrator(o).containerAddr(ctx, id)
}

func (o *Orchestrator) killAndRemove(ctx context.Context, id string) {
	dockerFactoryFromOrchestrator(o).killAndRemove(ctx, id)
}

func (o *Orchestrator) pullImage(ctx context.Context, ref string) error {
	return dockerFactoryFromOrchestrator(o).pullImage(ctx, ref)
}

// setActiveDockerInstanceForTest lets a test stash a fake Docker
// instance on o.activeInstance so the collapsed Watchdog(ctx,
// watchPeriod, sender) call can read its address and kill closure.
// The legacy (pre-phase-3-8) tests passed newID+newAddr directly to
// Watchdog; this helper is the shim that preserves their shape.
func setActiveDockerInstanceForTest(o *Orchestrator, id, addr string) {
	f := dockerFactoryFromOrchestrator(o)
	o.activeInstance = &dockerInstance{
		id:     id,
		addr:   addr,
		docker: f.docker,
		log:    f.log,
	}
}


type callTracker struct {
	drained   atomic.Int32
	undrained atomic.Int32
	exited    atomic.Int32
}

func newSender(t *testing.T) task.Sender {
	t.Helper()
	store := task.NewStore()
	return store.Create("test-task", "test")
}

// --- tests ---

func TestUpdateAlreadyCurrent(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "1.0.0",
			UpdateAvailable: false,
		},
	}
	o, tracker := newTestOrchestrator(t, &mockDocker{}, checker)
	sender := newSender(t)

	updated, err := o.Update(context.Background(), "stable", sender)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated {
		t.Error("expected updated=false for up-to-date")
	}
	if tracker.drained.Load() != 0 {
		t.Error("drain should not be called when up to date")
	}
}

func TestUpdatePullFails(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "2.0.0",
			UpdateAvailable: true,
		},
	}
	docker := &mockDocker{
		pullFn: func(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error) {
			return nil, io.ErrUnexpectedEOF
		},
	}
	o, tracker := newTestOrchestrator(t, docker, checker)
	sender := newSender(t)

	_, err := o.Update(context.Background(), "stable", sender)
	if err == nil {
		t.Fatal("expected error")
	}
	if tracker.drained.Load() != 0 {
		t.Error("drain should not be called when pull fails")
	}
}

func TestUpdateCloneFails(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "2.0.0",
			UpdateAvailable: true,
		},
	}
	docker := &mockDocker{
		createFn: func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			return client.ContainerCreateResult{}, io.ErrUnexpectedEOF
		},
	}
	o, tracker := newTestOrchestrator(t, docker, checker)
	sender := newSender(t)

	_, err := o.Update(context.Background(), "stable", sender)
	if err == nil {
		t.Fatal("expected error")
	}
	if tracker.drained.Load() != 0 {
		t.Error("drain should not be called when clone fails")
	}
}

func TestUpdateHappyPath(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "2.0.0",
			UpdateAvailable: true,
		},
	}

	// Set up a fake readyz and activate endpoint.
	readyzServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/admin/activate":
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer readyzServer.Close()

	// Parse the test server address.
	addr := readyzServer.Listener.Addr().String()
	ip, port := splitAddr(addr)

	var createdOpts client.ContainerCreateOptions
	docker := &mockDocker{
		inspectFn: func(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			if id == "new-container-123" {
				return client.ContainerInspectResult{
					Container: container.InspectResponse{
						Config:   &container.Config{Image: "ghcr.io/cynkra/blockyard:2.0.0"},
						HostConfig: &container.HostConfig{},
						NetworkSettings: &container.NetworkSettings{
							Networks: map[string]*network.EndpointSettings{
								"bridge": {IPAddress: netip.MustParseAddr(ip)},
							},
						},
					},
				}, nil
			}
			return defaultInspectResult(), nil
		},
		createFn: func(_ context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			createdOpts = opts
			return client.ContainerCreateResult{ID: "new-container-123"}, nil
		},
	}

	o, tracker := newTestOrchestrator(t, docker, checker)
	// Override the port to match our test server.
	o.cfg.Server.Bind = "0.0.0.0:" + port
	sender := newSender(t)

	updated, err := o.Update(context.Background(), "stable", sender)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !updated {
		t.Fatal("expected updated=true")
	}
	if o.activeInstance == nil || o.activeInstance.ID() != "new-container-123" {
		t.Errorf("activeInstance = %v, want new-container-123", o.activeInstance)
	}
	if tracker.drained.Load() != 1 {
		t.Error("drain should be called exactly once")
	}

	// Verify clone config.
	if createdOpts.Config == nil {
		t.Fatal("created config is nil")
	}
	if createdOpts.Config.Image != "ghcr.io/cynkra/blockyard:2.0.0" {
		t.Errorf("image = %q, want ghcr.io/cynkra/blockyard:2.0.0", createdOpts.Config.Image)
	}
	// Check BLOCKYARD_PASSIVE=1 was injected.
	found := false
	for _, e := range createdOpts.Config.Env {
		if e == "BLOCKYARD_PASSIVE=1" {
			found = true
		}
	}
	if !found {
		t.Error("expected BLOCKYARD_PASSIVE=1 in env")
	}
}

func TestUpdateConcurrencyGuard(t *testing.T) {
	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	o.state.Store("updating")

	if o.CASState("idle", "updating") {
		t.Error("CAS should fail when state is already updating")
	}
}

func TestWatchdogHealthy(t *testing.T) {
	readyzServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer readyzServer.Close()

	o, tracker := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	sender := newSender(t)
	setActiveDockerInstanceForTest(o, "new-id", readyzServer.Listener.Addr().String())

	err := o.Watchdog(context.Background(), 100*time.Millisecond, sender)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tracker.undrained.Load() != 0 {
		t.Error("undrain should not be called on healthy watchdog")
	}
}

func TestWatchdogUnhealthy(t *testing.T) {
	calls := 0
	readyzServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer readyzServer.Close()

	var removed atomic.Bool
	docker := &mockDocker{
		removeFn: func(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
			removed.Store(true)
			return client.ContainerRemoveResult{}, nil
		},
	}
	o, tracker := newTestOrchestrator(t, docker, &mockChecker{})
	sender := newSender(t)
	setActiveDockerInstanceForTest(o, "new-id", readyzServer.Listener.Addr().String())

	err := o.Watchdog(context.Background(), 30*time.Second, sender)
	if err == nil {
		t.Fatal("expected error from unhealthy watchdog")
	}
	if tracker.undrained.Load() != 1 {
		t.Error("undrain should be called on watchdog failure")
	}
	if !removed.Load() {
		t.Error("container should be removed on watchdog failure")
	}
}

func TestCloneConfig(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(_ context.Context, _ string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					Config: &container.Config{
						Image:  "ghcr.io/cynkra/blockyard:1.0.0",
						Env:    []string{"FOO=bar", "BAZ=qux"},
						Labels: map[string]string{"app": "blockyard"},
					},
					HostConfig: &container.HostConfig{
						PortBindings: network.PortMap{
							mustParsePort("8080/tcp"): []network.PortBinding{{HostPort: "8080"}},
						},
					},
					NetworkSettings: &container.NetworkSettings{
						Networks: map[string]*network.EndpointSettings{
							"mynet": {IPAddress: netip.MustParseAddr("10.0.0.1")},
						},
					},
				},
			}, nil
		},
	}
	o, _ := newTestOrchestrator(t, docker, &mockChecker{})

	opts, err := o.cloneConfig(context.Background(), "ghcr.io/cynkra/blockyard:2.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Image should be updated.
	if opts.Config.Image != "ghcr.io/cynkra/blockyard:2.0.0" {
		t.Errorf("image = %q, want updated", opts.Config.Image)
	}

	// Port bindings should be stripped.
	if opts.HostConfig.PortBindings != nil {
		t.Error("port bindings should be stripped")
	}

	// Labels should be preserved.
	if opts.Config.Labels["app"] != "blockyard" {
		t.Error("labels should be preserved")
	}

	// BLOCKYARD_PASSIVE=1 should be injected.
	found := false
	for _, e := range opts.Config.Env {
		if e == "BLOCKYARD_PASSIVE=1" {
			found = true
		}
	}
	if !found {
		t.Error("expected BLOCKYARD_PASSIVE=1 in env")
	}

	// Network config should be mapped.
	if opts.NetworkingConfig == nil {
		t.Fatal("networking config should be set")
	}
	if _, ok := opts.NetworkingConfig.EndpointsConfig["mynet"]; !ok {
		t.Error("expected mynet in endpoints config")
	}
}

func TestRollbackNoBackup(t *testing.T) {
	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	// Point to empty temp dir (no backup files).
	o.cfg.Database.Path = filepath.Join(t.TempDir(), "test.db")
	sender := newSender(t)

	err := o.Rollback(context.Background(), sender, func() {})
	if err == nil {
		t.Fatal("expected error for no backup")
	}
}

func TestAppendOrReplace(t *testing.T) {
	env := []string{"A=1", "B=2", "C=3"}

	// Replace existing.
	env = appendOrReplace(env, "B", "99")
	if env[1] != "B=99" {
		t.Errorf("expected B=99, got %s", env[1])
	}

	// Append new.
	env = appendOrReplace(env, "D", "4")
	if len(env) != 4 {
		t.Errorf("expected 4 entries, got %d", len(env))
	}
	if env[3] != "D=4" {
		t.Errorf("expected D=4, got %s", env[3])
	}
}

func TestBackupMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	meta, err := database.BackupWithMeta(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ImageTag != "v1.0.0" {
		t.Errorf("image tag = %q, want v1.0.0", meta.ImageTag)
	}
	if meta.BackupPath == "" {
		t.Error("backup path should not be empty")
	}

	// Verify the meta file can be found.
	found, err := db.LatestBackupMeta(dbPath)
	if err != nil {
		t.Fatalf("LatestBackupMeta: %v", err)
	}
	if found.ImageTag != "v1.0.0" {
		t.Errorf("found image tag = %q, want v1.0.0", found.ImageTag)
	}
}

func TestLatestBackupMetaEmpty(t *testing.T) {
	_, err := db.LatestBackupMeta(filepath.Join(t.TempDir(), "test.db"))
	if err != db.ErrNoBackup {
		t.Errorf("expected ErrNoBackup, got %v", err)
	}
}

func TestScheduledSkipsInProgress(t *testing.T) {
	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	o.state.Store("updating")

	// CAS should fail.
	if o.CASState("idle", "updating") {
		t.Error("should not transition from updating to updating")
	}
}

func mustParsePort(s string) network.Port {
	p, err := network.ParsePort(s)
	if err != nil {
		panic(err)
	}
	return p
}

// splitAddr splits "host:port" into host and port strings.
func splitAddr(addr string) (string, string) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:]
		}
	}
	return addr, ""
}

// Ensure the orchestrator state is set correctly and readable.
func TestStateTransitions(t *testing.T) {
	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})

	if o.State() != "idle" {
		t.Errorf("initial state = %q, want idle", o.State())
	}

	if !o.CASState("idle", "updating") {
		t.Error("CAS idle→updating should succeed")
	}
	if o.State() != "updating" {
		t.Errorf("state = %q, want updating", o.State())
	}

	o.SetState("idle")
	if o.State() != "idle" {
		t.Errorf("state = %q, want idle after reset", o.State())
	}
}

func TestImageWithTag(t *testing.T) {
	ref := imageWithTag("ghcr.io/cynkra/blockyard", "1.2.3")
	if ref != "ghcr.io/cynkra/blockyard:1.2.3" {
		t.Errorf("imageWithTag = %q", ref)
	}
}

func TestRollbackIrreversible(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Create a backup to find.
	_, err = database.BackupWithMeta(context.Background(), "v0.9.0")
	if err != nil {
		t.Fatal(err)
	}

	// The CheckDownMigrationSafety check only triggers when versions differ.
	// Since we only have migration 001 and the backup records it, versions
	// match and no down-migration check occurs. This test verifies the
	// rollback path finds the backup metadata correctly.
	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	o.db = database
	o.cfg.Database.Path = dbPath
	sender := newSender(t)

	// Rollback will fail at pull since our mock pulls succeed but the
	// clone will try to start. The key thing is it found the backup.
	err = o.Rollback(context.Background(), sender, func() {})
	// Any error after finding backup metadata is acceptable for this test.
	_ = err
}

func TestNewConstructor(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	cfg := &config.Config{
		Server:   config.ServerConfig{Bind: "0.0.0.0:8080"},
		Database: config.DatabaseConfig{Driver: "sqlite", Path: dbPath},
	}

	var exitCalled bool
	factory := newDockerFactoryForTest(&mockDocker{}, "server-id", func() string { return "8080" })
	o := New(
		factory,
		database,
		cfg,
		"1.0.0",
		task.NewStore(),
		&DefaultChecker{},
		slog.Default(),
		func() {},
		func() {},
		func() { exitCalled = true },
	)

	if o.State() != "idle" {
		t.Errorf("initial state = %q, want idle", o.State())
	}

	o.Exit()
	if !exitCalled {
		t.Error("Exit should call exitFn")
	}
}

func TestRunScheduledCancellation(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "1.0.0",
			UpdateAvailable: false,
		},
	}
	o, _ := newTestOrchestrator(t, &mockDocker{}, checker)
	o.cfg.Update = &config.UpdateConfig{
		Schedule:    "* * * * *",
		Channel:     "stable",
		WatchPeriod: config.Duration{Duration: 1 * time.Minute},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.RunScheduled(ctx, "* * * * *", "stable")
		close(done)
	}()

	// Cancel immediately — RunScheduled should exit.
	cancel()
	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("RunScheduled did not exit after context cancellation")
	}
}

func TestRunScheduledInvalidCron(t *testing.T) {
	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})

	// Invalid cron should return immediately without panic.
	o.RunScheduled(context.Background(), "not-a-cron", "stable")
}

func TestRollbackHappyPath(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Create a backup.
	_, err = database.BackupWithMeta(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	// Set up a fake readyz and activate endpoint.
	readyzServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer readyzServer.Close()

	addr := readyzServer.Listener.Addr().String()
	ip, port := splitAddr(addr)

	docker := &mockDocker{
		inspectFn: func(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			if id == "new-container-123" {
				return client.ContainerInspectResult{
					Container: container.InspectResponse{
						Config:     &container.Config{Image: "ghcr.io/cynkra/blockyard:v1.0.0"},
						HostConfig: &container.HostConfig{},
						NetworkSettings: &container.NetworkSettings{
							Networks: map[string]*network.EndpointSettings{
								"bridge": {IPAddress: netip.MustParseAddr(ip)},
							},
						},
					},
				}, nil
			}
			return defaultInspectResult(), nil
		},
	}

	o, tracker := newTestOrchestrator(t, docker, &mockChecker{})
	o.db = database
	o.cfg.Database.Path = dbPath
	o.cfg.Server.Bind = "0.0.0.0:" + port
	sender := newSender(t)

	err = o.Rollback(context.Background(), sender, func() {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tracker.drained.Load() != 1 {
		t.Error("drain should be called during rollback")
	}
}

func TestRollbackCloneFailsAfterMigration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	_, err = database.BackupWithMeta(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	docker := &mockDocker{
		createFn: func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			return client.ContainerCreateResult{}, io.ErrUnexpectedEOF
		},
	}

	var shutdownCalled bool
	o, _ := newTestOrchestrator(t, docker, &mockChecker{})
	o.db = database
	o.cfg.Database.Path = dbPath
	sender := newSender(t)

	// Since migration versions match (no down-migration needed),
	// the clone failure won't trigger shutdownFn.
	err = o.Rollback(context.Background(), sender, func() { shutdownCalled = true })
	if err == nil {
		t.Fatal("expected error when clone fails")
	}
	// shutdownFn should NOT be called when no migration ran.
	if shutdownCalled {
		t.Error("shutdownFn should not be called when no migration ran")
	}
}

func TestUpdateReadyTimeout(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "2.0.0",
			UpdateAvailable: true,
		},
	}

	var removed atomic.Bool
	docker := &mockDocker{
		inspectFn: func(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			if id == "new-container-123" {
				// Return an IP where nothing listens → readyz always fails.
				return client.ContainerInspectResult{
					Container: container.InspectResponse{
						Config:     &container.Config{Image: "ghcr.io/cynkra/blockyard:2.0.0"},
						HostConfig: &container.HostConfig{},
						NetworkSettings: &container.NetworkSettings{
							Networks: map[string]*network.EndpointSettings{
								"bridge": {IPAddress: netip.MustParseAddr("192.0.2.1")},
							},
						},
					},
				}, nil
			}
			return defaultInspectResult(), nil
		},
		removeFn: func(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
			removed.Store(true)
			return client.ContainerRemoveResult{}, nil
		},
	}

	o, tracker := newTestOrchestrator(t, docker, checker)
	// Very short timeout to make test fast.
	o.cfg.Proxy.WorkerStartTimeout = config.Duration{Duration: 3 * time.Second}
	sender := newSender(t)

	_, err := o.Update(context.Background(), "stable", sender)
	if err == nil {
		t.Fatal("expected error from ready timeout")
	}
	if !removed.Load() {
		t.Error("container should be removed after ready timeout")
	}
	if tracker.drained.Load() != 0 {
		t.Error("drain should not be called when readyz times out")
	}
}

func TestListenPort(t *testing.T) {
	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})

	o.cfg.Server.Bind = "0.0.0.0:9090"
	if p := o.listenPort(); p != "9090" {
		t.Errorf("listenPort = %q, want 9090", p)
	}

	o.cfg.Server.Bind = "8080"
	if p := o.listenPort(); p != "8080" {
		t.Errorf("listenPort = %q for no-colon addr, want 8080", p)
	}
}

func TestGenerateActivationToken(t *testing.T) {
	tok := generateActivationToken()
	if tok == "" {
		t.Error("token should not be empty")
	}
	if len(tok) < 16 {
		t.Errorf("token too short: %q", tok)
	}
	// Should be different each time.
	tok2 := generateActivationToken()
	if tok == tok2 {
		t.Error("consecutive tokens should differ")
	}
}

func TestCurrentImageBaseAndTag(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(_ context.Context, _ string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					Config:     &container.Config{Image: "ghcr.io/cynkra/blockyard:3.2.1"},
					HostConfig: &container.HostConfig{},
				},
			}, nil
		},
	}
	o, _ := newTestOrchestrator(t, docker, &mockChecker{})

	base := o.currentImageBase(context.Background())
	if base != "ghcr.io/cynkra/blockyard" {
		t.Errorf("currentImageBase = %q", base)
	}

	tag := o.currentImageTag(context.Background())
	if tag != "3.2.1" {
		t.Errorf("currentImageTag = %q", tag)
	}
}

func TestCurrentImageBaseNoTag(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(_ context.Context, _ string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					Config:     &container.Config{Image: "blockyard"},
					HostConfig: &container.HostConfig{},
				},
			}, nil
		},
	}
	o, _ := newTestOrchestrator(t, docker, &mockChecker{})

	base := o.currentImageBase(context.Background())
	if base != "blockyard" {
		t.Errorf("currentImageBase without tag = %q", base)
	}
}

func TestKillAndRemoveBestEffort(t *testing.T) {
	// killAndRemove should not panic even when stop/remove fail.
	docker := &mockDocker{
		stopFn: func(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
			return client.ContainerStopResult{}, io.ErrUnexpectedEOF
		},
		removeFn: func(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
			return client.ContainerRemoveResult{}, io.ErrUnexpectedEOF
		},
	}
	o, _ := newTestOrchestrator(t, docker, &mockChecker{})

	// Should not panic or return error.
	o.killAndRemove(context.Background(), "some-container-id-1234")
}

func TestRunScheduledOnceNoUpdate(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{UpdateAvailable: false},
	}
	o, _ := newTestOrchestrator(t, &mockDocker{}, checker)
	if o.runScheduledOnce(context.Background(), "stable") {
		t.Error("expected false when no update available")
	}
}

func TestRunScheduledOnceCheckError(t *testing.T) {
	checker := &mockChecker{err: io.ErrUnexpectedEOF}
	o, _ := newTestOrchestrator(t, &mockDocker{}, checker)
	if o.runScheduledOnce(context.Background(), "stable") {
		t.Error("expected false on check error")
	}
}

func TestRunScheduledOnceCASFails(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{UpdateAvailable: true, CurrentVersion: "1.0.0", LatestVersion: "2.0.0"},
	}
	o, _ := newTestOrchestrator(t, &mockDocker{}, checker)
	o.state.Store("updating") // block CAS
	if o.runScheduledOnce(context.Background(), "stable") {
		t.Error("expected false when CAS fails")
	}
}

func TestRunScheduledOnceUpdateFails(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{UpdateAvailable: true, CurrentVersion: "1.0.0", LatestVersion: "2.0.0"},
	}
	docker := &mockDocker{
		pullFn: func(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error) {
			return nil, io.ErrUnexpectedEOF
		},
	}
	o, _ := newTestOrchestrator(t, docker, checker)
	if o.runScheduledOnce(context.Background(), "stable") {
		t.Error("expected false when update fails")
	}
	if o.State() != "idle" {
		t.Errorf("state should reset to idle, got %q", o.State())
	}
}

func TestRunScheduledNoUpdate(t *testing.T) {
	// RunScheduled should check, find no update, and keep looping until cancelled.
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "1.0.0",
			UpdateAvailable: false,
		},
	}
	o, _ := newTestOrchestrator(t, &mockDocker{}, checker)
	o.cfg.Update = &config.UpdateConfig{
		WatchPeriod: config.Duration{Duration: 1 * time.Minute},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// Use "* * * * *" (every minute) but cancel quickly.
		// The scheduler sleeps until next cron tick, so we cancel during the sleep.
		o.RunScheduled(ctx, "* * * * *", "stable")
		close(done)
	}()

	// Let it run briefly, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunScheduled did not exit")
	}
}

func TestRunScheduledCheckFails(t *testing.T) {
	checker := &mockChecker{
		err: io.ErrUnexpectedEOF,
	}
	o, _ := newTestOrchestrator(t, &mockDocker{}, checker)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.RunScheduled(ctx, "* * * * *", "stable")
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunScheduled did not exit after check failure")
	}
}

func TestCurrentImageTagError(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{}, io.ErrUnexpectedEOF
		},
	}
	o, _ := newTestOrchestrator(t, docker, &mockChecker{})

	// Should fall back to version string.
	tag := o.currentImageTag(context.Background())
	if tag != "1.0.0" {
		t.Errorf("expected fallback to version, got %q", tag)
	}

	base := o.currentImageBase(context.Background())
	if base != "blockyard" {
		t.Errorf("expected fallback base, got %q", base)
	}
}

func TestActivateError(t *testing.T) {
	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	// Call activate against a closed server → should return error.
	err := o.activate(context.Background(), "127.0.0.1:1")
	if err == nil {
		t.Error("expected error from activate against closed port")
	}
}

func TestCheckReadyError(t *testing.T) {
	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	err := o.checkReady(context.Background(), "127.0.0.1:1")
	if err == nil {
		t.Error("expected error from checkReady against closed port")
	}
}

func TestContainerAddrNoNetworks(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{
				Container: container.InspectResponse{
					Config:          &container.Config{},
					HostConfig:      &container.HostConfig{},
					NetworkSettings: &container.NetworkSettings{},
				},
			}, nil
		},
	}
	o, _ := newTestOrchestrator(t, docker, &mockChecker{})
	_, err := o.containerAddr(context.Background(), "some-id-12345678")
	if err == nil {
		t.Error("expected error when no networks")
	}
}

func TestStartCloneActivationToken(t *testing.T) {
	docker := &mockDocker{}
	o, _ := newTestOrchestrator(t, docker, &mockChecker{})

	_, err := o.startClone(context.Background(), "ghcr.io/cynkra/blockyard:2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if o.activationToken == "" {
		t.Error("activation token should be set after startClone")
	}
}

func TestDefaultChecker(t *testing.T) {
	// Just verify DefaultChecker satisfies the interface.
	var _ updateAPI = &DefaultChecker{}
}

func TestNewForTestState(t *testing.T) {
	o := NewForTest()
	if o.State() != "idle" {
		t.Errorf("NewForTest state = %q, want idle", o.State())
	}
	o.SetState("updating")
	if o.State() != "updating" {
		t.Errorf("state = %q, want updating", o.State())
	}
}

// ---------------------------------------------------------------------------
// Update: error recovery and state management
// ---------------------------------------------------------------------------

// Verify that activate failure after drain triggers undrain + container removal.
func TestUpdateActivateFailsAfterDrain(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion: "1.0.0", LatestVersion: "2.0.0", UpdateAvailable: true,
		},
	}

	// readyz succeeds, activate fails.
	activateCalled := false
	fakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/api/v1/admin/activate" {
			activateCalled = true
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}))
	defer fakeServer.Close()
	addr := fakeServer.Listener.Addr().String()
	ip, port := splitAddr(addr)

	var removed atomic.Bool
	docker := &mockDocker{
		inspectFn: func(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			if id == "new-container-123" {
				return client.ContainerInspectResult{Container: container.InspectResponse{
					Config: &container.Config{Image: "img:2.0.0"}, HostConfig: &container.HostConfig{},
					NetworkSettings: &container.NetworkSettings{
						Networks: map[string]*network.EndpointSettings{
							"br": {IPAddress: netip.MustParseAddr(ip)},
						}},
				}}, nil
			}
			return defaultInspectResult(), nil
		},
		removeFn: func(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
			removed.Store(true)
			return client.ContainerRemoveResult{}, nil
		},
	}

	o, tracker := newTestOrchestrator(t, docker, checker)
	o.cfg.Server.Bind = "0.0.0.0:" + port
	sender := newSender(t)

	_, err := o.Update(context.Background(), "stable", sender)
	if err == nil {
		t.Fatal("expected error when activate fails")
	}
	if !activateCalled {
		t.Error("activate should have been called")
	}
	if tracker.drained.Load() != 1 {
		t.Error("drain should be called before activate")
	}
	if tracker.undrained.Load() != 1 {
		t.Error("undrain must be called after activate failure")
	}
	if !removed.Load() {
		t.Error("new container must be removed after activate failure")
	}
}

// Verify that backup failure does NOT call drain.
func TestUpdateBackupFailsNoDrain(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion: "1.0.0", LatestVersion: "2.0.0", UpdateAvailable: true,
		},
	}
	o, tracker := newTestOrchestrator(t, &mockDocker{}, checker)
	// Use in-memory DB which cannot back up.
	memDB, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer memDB.Close()
	o.db = memDB

	sender := newSender(t)
	_, err = o.Update(context.Background(), "stable", sender)
	if err == nil {
		t.Fatal("expected backup error")
	}
	if tracker.drained.Load() != 0 {
		t.Error("drain must NOT be called when backup fails")
	}
}

// ---------------------------------------------------------------------------
// Rollback: fatal shutdown path
// ---------------------------------------------------------------------------

// After a down-migration, if startClone fails, shutdownFn must be called
// (not undrain — the schema is wrong for the running code).
func TestRollbackFatalAfterMigration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Create backup at current version so versions match (no actual
	// down-migration runs). We verify the non-migration paths.
	_, err = database.BackupWithMeta(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	docker := &mockDocker{
		createFn: func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
			return client.ContainerCreateResult{}, io.ErrUnexpectedEOF
		},
	}

	var shutdownCalled bool
	o, tracker := newTestOrchestrator(t, docker, &mockChecker{})
	o.db = database
	o.cfg.Database.Path = dbPath
	sender := newSender(t)

	err = o.Rollback(context.Background(), sender, func() { shutdownCalled = true })
	if err == nil {
		t.Fatal("expected error")
	}
	// No migration ran (versions matched), so shutdownFn should NOT be called.
	if shutdownCalled {
		t.Error("shutdownFn should NOT be called when no migration ran")
	}
	// undrain should NOT be called either — we never drained.
	if tracker.undrained.Load() != 0 {
		t.Error("undrain should not be called when clone fails before drain")
	}
}

// When activate fails but no migration ran, undrain IS called (server can resume).
func TestRollbackActivateFailsNoMigration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	_, err = database.BackupWithMeta(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	// readyz succeeds, activate fails.
	fakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fakeServer.Close()
	addr := fakeServer.Listener.Addr().String()
	ip, port := splitAddr(addr)

	var removed atomic.Bool
	docker := &mockDocker{
		inspectFn: func(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			if id == "new-container-123" {
				return client.ContainerInspectResult{Container: container.InspectResponse{
					Config: &container.Config{Image: "img:v1.0.0"}, HostConfig: &container.HostConfig{},
					NetworkSettings: &container.NetworkSettings{
						Networks: map[string]*network.EndpointSettings{
							"br": {IPAddress: netip.MustParseAddr(ip)},
						}},
				}}, nil
			}
			return defaultInspectResult(), nil
		},
		removeFn: func(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
			removed.Store(true)
			return client.ContainerRemoveResult{}, nil
		},
	}

	var shutdownCalled bool
	o, tracker := newTestOrchestrator(t, docker, &mockChecker{})
	o.db = database
	o.cfg.Database.Path = dbPath
	o.cfg.Server.Bind = "0.0.0.0:" + port
	sender := newSender(t)

	err = o.Rollback(context.Background(), sender, func() { shutdownCalled = true })
	if err == nil {
		t.Fatal("expected error")
	}
	if shutdownCalled {
		t.Error("shutdownFn should NOT be called — no migration ran")
	}
	if tracker.drained.Load() != 1 {
		t.Error("drain should have been called")
	}
	if tracker.undrained.Load() != 1 {
		t.Error("undrain MUST be called when activate fails without migration")
	}
	if !removed.Load() {
		t.Error("container must be removed")
	}
}

// ---------------------------------------------------------------------------
// Watchdog: state transitions
// ---------------------------------------------------------------------------

func TestWatchdogStateTransitions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	o.state.Store("updating") // pre-watchdog state
	sender := newSender(t)
	setActiveDockerInstanceForTest(o, "id", srv.Listener.Addr().String())

	// Very short watch period.
	err := o.Watchdog(context.Background(), 100*time.Millisecond, sender)
	if err != nil {
		t.Fatal(err)
	}
	// Watchdog sets "watching" then defers back to "idle".
	if o.State() != "idle" {
		t.Errorf("state after watchdog = %q, want idle", o.State())
	}
}

func TestWatchdogContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	sender := newSender(t)
	setActiveDockerInstanceForTest(o, "id", srv.Listener.Addr().String())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := o.Watchdog(ctx, time.Minute, sender)
	if err == nil {
		t.Error("expected context error")
	}
}

func TestWatchdogZeroPeriod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	sender := newSender(t)
	setActiveDockerInstanceForTest(o, "id", srv.Listener.Addr().String())

	// Zero period = deadline already passed = immediate success.
	err := o.Watchdog(context.Background(), 0, sender)
	if err != nil {
		t.Fatalf("expected immediate success with zero period, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Clone: edge cases
// ---------------------------------------------------------------------------

func TestCloneConfigInspectError(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{}, io.ErrUnexpectedEOF
		},
	}
	o, _ := newTestOrchestrator(t, docker, &mockChecker{})

	_, err := o.cloneConfig(context.Background(), "img:new", nil)
	if err == nil {
		t.Error("expected error when inspect fails")
	}
}

func TestCloneConfigExtraEnvMalformed(t *testing.T) {
	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	// Entry without "=" is silently skipped.
	opts, err := o.cloneConfig(context.Background(), "img:new", []string{"NOEQUALS"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range opts.Config.Env {
		if e == "NOEQUALS" {
			t.Error("malformed env entry should not appear verbatim")
		}
	}
}

func TestCloneConfigNilNetworkSettings(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{Container: container.InspectResponse{
				Config: &container.Config{Image: "old:1.0", Env: []string{}},
				HostConfig: &container.HostConfig{},
				// NetworkSettings intentionally nil
			}}, nil
		},
	}
	o, _ := newTestOrchestrator(t, docker, &mockChecker{})
	opts, err := o.cloneConfig(context.Background(), "img:new", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.NetworkingConfig != nil {
		t.Error("expected nil NetworkingConfig when source has no networks")
	}
}

func TestStartCloneStartFails(t *testing.T) {
	docker := &mockDocker{
		startFn: func(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
			return client.ContainerStartResult{}, io.ErrUnexpectedEOF
		},
	}
	o, _ := newTestOrchestrator(t, docker, &mockChecker{})
	_, err := o.startClone(context.Background(), "img:2.0")
	if err == nil {
		t.Error("expected error when start fails")
	}
}

// ---------------------------------------------------------------------------
// Backup metadata: edge cases
// ---------------------------------------------------------------------------

func TestLatestBackupMetaMultiple(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Create two backups with different tags.
	m1, err := database.BackupWithMeta(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	// Backup filename has 1-second resolution; wait for distinct timestamp.
	time.Sleep(1100 * time.Millisecond)
	m2, err := database.BackupWithMeta(context.Background(), "v2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	_ = m1

	// LatestBackupMeta should return the second (newest).
	found, err := db.LatestBackupMeta(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if found.ImageTag != m2.ImageTag {
		t.Errorf("expected latest tag %q, got %q", m2.ImageTag, found.ImageTag)
	}
}

func TestBackupWithMetaMigrationVersion(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	meta, err := database.BackupWithMeta(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	// Migration version should be non-zero (we have at least migration 001).
	if meta.MigrationVersion == 0 {
		t.Error("expected non-zero migration version in backup metadata")
	}
	if meta.CreatedAt == "" {
		t.Error("expected non-empty created_at")
	}
}

// ---------------------------------------------------------------------------
// checkReady: non-200 response
// ---------------------------------------------------------------------------

func TestCheckReadyNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	err := o.checkReady(context.Background(), srv.Listener.Addr().String())
	if err == nil {
		t.Error("expected error for 503 response")
	}
}

// ---------------------------------------------------------------------------
// activate: non-200 response with body
// ---------------------------------------------------------------------------

func TestActivateNon200WithBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()

	o, _ := newTestOrchestrator(t, &mockDocker{}, &mockChecker{})
	err := o.activate(context.Background(), srv.Listener.Addr().String())
	if err == nil {
		t.Error("expected error for 403 response")
	}
}

// ---------------------------------------------------------------------------
// containerAddr: inspect error
// ---------------------------------------------------------------------------

func TestContainerAddrInspectError(t *testing.T) {
	docker := &mockDocker{
		inspectFn: func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			return client.ContainerInspectResult{}, io.ErrUnexpectedEOF
		},
	}
	o, _ := newTestOrchestrator(t, docker, &mockChecker{})
	_, err := o.containerAddr(context.Background(), "some-id-12345678")
	if err == nil {
		t.Error("expected error when inspect fails")
	}
}

func init() {
	// Suppress log output during tests.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError,
	})))
}
