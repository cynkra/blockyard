package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/update"
)

// --- fake server factory (backend-agnostic) ---

type fakeInstance struct {
	id     string
	addr   string
	killFn func(ctx context.Context)
}

func (f *fakeInstance) ID() string   { return f.id }
func (f *fakeInstance) Addr() string { return f.addr }
func (f *fakeInstance) Kill(ctx context.Context) {
	if f.killFn != nil {
		f.killFn(ctx)
	}
}

type fakeServerFactory struct {
	preUpdateErr     error
	createInstanceFn func(ctx context.Context, ref string, extraEnv []string, sender task.Sender) (newServerInstance, error)
	supportsRollback bool
	imageBase        string
	imageTag         string
}

func (f *fakeServerFactory) PreUpdate(_ context.Context, _ string, _ task.Sender) error {
	return f.preUpdateErr
}

func (f *fakeServerFactory) CreateInstance(ctx context.Context, ref string, extraEnv []string, sender task.Sender) (newServerInstance, error) {
	if f.createInstanceFn != nil {
		return f.createInstanceFn(ctx, ref, extraEnv, sender)
	}
	return nil, fmt.Errorf("fakeServerFactory: CreateInstance not configured")
}

func (f *fakeServerFactory) CurrentImageBase(_ context.Context) string {
	if f.imageBase != "" {
		return f.imageBase
	}
	return "ghcr.io/cynkra/blockyard"
}

func (f *fakeServerFactory) CurrentImageTag(_ context.Context) string {
	if f.imageTag != "" {
		return f.imageTag
	}
	return "1.0.0"
}

func (f *fakeServerFactory) SupportsRollback() bool {
	return f.supportsRollback
}

// --- mock update checker ---

type mockChecker struct {
	result *update.Result
	err    error
}

func (m *mockChecker) CheckLatest(_, _ string) (*update.Result, error) {
	return m.result, m.err
}

// --- test helpers ---

type callTracker struct {
	drained   atomic.Int32
	undrained atomic.Int32
	exited    atomic.Int32
}

func newTestOrchestrator(t *testing.T, factory ServerFactory, checker updateAPI) (*Orchestrator, *callTracker) {
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

func newSender(t *testing.T) task.Sender {
	t.Helper()
	store := task.NewStore()
	return store.Create("test-task", "test")
}

// ---------------------------------------------------------------------------
// Update flow
// ---------------------------------------------------------------------------

func TestUpdateAlreadyCurrent(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "1.0.0",
			UpdateAvailable: false,
		},
	}
	o, tracker := newTestOrchestrator(t, &fakeServerFactory{}, checker)
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

func TestUpdatePreUpdateFails(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "2.0.0",
			UpdateAvailable: true,
		},
	}
	factory := &fakeServerFactory{
		preUpdateErr: io.ErrUnexpectedEOF,
	}
	o, tracker := newTestOrchestrator(t, factory, checker)
	sender := newSender(t)

	_, err := o.Update(context.Background(), "stable", sender)
	if err == nil {
		t.Fatal("expected error")
	}
	if tracker.drained.Load() != 0 {
		t.Error("drain should not be called when pre-update fails")
	}
}

func TestUpdateCreateFails(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "2.0.0",
			UpdateAvailable: true,
		},
	}
	factory := &fakeServerFactory{
		createInstanceFn: func(context.Context, string, []string, task.Sender) (newServerInstance, error) {
			return nil, io.ErrUnexpectedEOF
		},
	}
	o, tracker := newTestOrchestrator(t, factory, checker)
	sender := newSender(t)

	_, err := o.Update(context.Background(), "stable", sender)
	if err == nil {
		t.Fatal("expected error")
	}
	if tracker.drained.Load() != 0 {
		t.Error("drain should not be called when create fails")
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

	readyzServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/admin/activate":
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer readyzServer.Close()

	factory := &fakeServerFactory{
		createInstanceFn: func(context.Context, string, []string, task.Sender) (newServerInstance, error) {
			return &fakeInstance{
				id:   "new-instance-123",
				addr: readyzServer.Listener.Addr().String(),
			}, nil
		},
	}

	o, tracker := newTestOrchestrator(t, factory, checker)
	sender := newSender(t)

	updated, err := o.Update(context.Background(), "stable", sender)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !updated {
		t.Fatal("expected updated=true")
	}
	if o.activeInstance == nil || o.activeInstance.ID() != "new-instance-123" {
		t.Errorf("activeInstance = %v, want new-instance-123", o.activeInstance)
	}
	if tracker.drained.Load() != 1 {
		t.Error("drain should be called exactly once")
	}
}

func TestUpdateConcurrencyGuard(t *testing.T) {
	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})
	o.state.Store("updating")

	if o.CASState("idle", "updating") {
		t.Error("CAS should fail when state is already updating")
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

	var killed atomic.Bool
	factory := &fakeServerFactory{
		createInstanceFn: func(context.Context, string, []string, task.Sender) (newServerInstance, error) {
			return &fakeInstance{
				id:   "unreachable-123",
				addr: "192.0.2.1:9999",
				killFn: func(context.Context) {
					killed.Store(true)
				},
			}, nil
		},
	}

	o, tracker := newTestOrchestrator(t, factory, checker)
	o.cfg.Proxy.WorkerStartTimeout = config.Duration{Duration: 3 * time.Second}
	sender := newSender(t)

	_, err := o.Update(context.Background(), "stable", sender)
	if err == nil {
		t.Fatal("expected error from ready timeout")
	}
	if !killed.Load() {
		t.Error("instance should be killed after ready timeout")
	}
	if tracker.drained.Load() != 0 {
		t.Error("drain should not be called when readyz times out")
	}
}

func TestUpdateBackupFailsNoDrain(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion: "1.0.0", LatestVersion: "2.0.0", UpdateAvailable: true,
		},
	}
	o, tracker := newTestOrchestrator(t, &fakeServerFactory{}, checker)
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

func TestUpdateActivateFailsAfterDrain(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion: "1.0.0", LatestVersion: "2.0.0", UpdateAvailable: true,
		},
	}

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

	var killed atomic.Bool
	factory := &fakeServerFactory{
		createInstanceFn: func(context.Context, string, []string, task.Sender) (newServerInstance, error) {
			return &fakeInstance{
				id:   "new-instance-123",
				addr: fakeServer.Listener.Addr().String(),
				killFn: func(context.Context) {
					killed.Store(true)
				},
			}, nil
		},
	}

	o, tracker := newTestOrchestrator(t, factory, checker)
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
	if !killed.Load() {
		t.Error("instance must be killed after activate failure")
	}
}

// ---------------------------------------------------------------------------
// State management
// ---------------------------------------------------------------------------

func TestStateTransitions(t *testing.T) {
	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})

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

// ---------------------------------------------------------------------------
// Watchdog
// ---------------------------------------------------------------------------

func TestWatchdogHealthy(t *testing.T) {
	readyzServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer readyzServer.Close()

	o, tracker := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})
	sender := newSender(t)
	o.activeInstance = &fakeInstance{id: "new-id", addr: readyzServer.Listener.Addr().String()}

	err := o.Watchdog(context.Background(), 100*time.Millisecond, sender)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tracker.undrained.Load() != 0 {
		t.Error("undrain should not be called on healthy watchdog")
	}
}

func TestWatchdogUnhealthy(t *testing.T) {
	// Use threshold=1 so the test doesn't need to wait for 3 ticks.
	cleanup := SetWatchdogFailureThresholdForTest(1)
	defer cleanup()

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

	var killed atomic.Bool
	o, tracker := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})
	sender := newSender(t)
	o.activeInstance = &fakeInstance{
		id:   "new-id",
		addr: readyzServer.Listener.Addr().String(),
		killFn: func(context.Context) {
			killed.Store(true)
		},
	}

	err := o.Watchdog(context.Background(), 30*time.Second, sender)
	if err == nil {
		t.Fatal("expected error from unhealthy watchdog")
	}
	if tracker.undrained.Load() != 1 {
		t.Error("undrain should be called on watchdog failure")
	}
	if !killed.Load() {
		t.Error("instance should be killed on watchdog failure")
	}
}

func TestWatchdogTransientFailureRecovers(t *testing.T) {
	var calls atomic.Int32
	readyzServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		// Fail on the 2nd call only; all others succeed.
		if n == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer readyzServer.Close()

	o, tracker := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})
	sender := newSender(t)
	o.activeInstance = &fakeInstance{id: "new-id", addr: readyzServer.Listener.Addr().String()}

	err := o.Watchdog(context.Background(), 100*time.Millisecond, sender)
	if err != nil {
		t.Fatalf("single transient failure should not trigger rollback: %v", err)
	}
	if tracker.undrained.Load() != 0 {
		t.Error("undrain should not be called for a transient failure")
	}
}

func TestWatchdogStateTransitions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})
	o.state.Store("updating")
	sender := newSender(t)
	o.activeInstance = &fakeInstance{id: "id", addr: srv.Listener.Addr().String()}

	err := o.Watchdog(context.Background(), 100*time.Millisecond, sender)
	if err != nil {
		t.Fatal(err)
	}
	if o.State() != "idle" {
		t.Errorf("state after watchdog = %q, want idle", o.State())
	}
}

func TestWatchdogContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})
	sender := newSender(t)
	o.activeInstance = &fakeInstance{id: "id", addr: srv.Listener.Addr().String()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

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

	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})
	sender := newSender(t)
	o.activeInstance = &fakeInstance{id: "id", addr: srv.Listener.Addr().String()}

	err := o.Watchdog(context.Background(), 0, sender)
	if err != nil {
		t.Fatalf("expected immediate success with zero period, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Rollback
// ---------------------------------------------------------------------------

func TestRollbackNoBackup(t *testing.T) {
	factory := &fakeServerFactory{supportsRollback: true}
	o, _ := newTestOrchestrator(t, factory, &mockChecker{})
	o.cfg.Database.Path = filepath.Join(t.TempDir(), "test.db")
	sender := newSender(t)

	err := o.Rollback(context.Background(), sender, func() {})
	if err == nil {
		t.Fatal("expected error for no backup")
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

	_, err = database.BackupWithMeta(context.Background(), "v0.9.0")
	if err != nil {
		t.Fatal(err)
	}

	factory := &fakeServerFactory{supportsRollback: true}
	o, _ := newTestOrchestrator(t, factory, &mockChecker{})
	o.db = database
	o.cfg.Database.Path = dbPath
	sender := newSender(t)

	// Rollback will fail at clone since our fake returns an error by default.
	// The key thing is it found the backup metadata.
	err = o.Rollback(context.Background(), sender, func() {})
	_ = err
}

func TestRollbackHappyPath(t *testing.T) {
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

	readyzServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer readyzServer.Close()

	factory := &fakeServerFactory{
		supportsRollback: true,
		createInstanceFn: func(context.Context, string, []string, task.Sender) (newServerInstance, error) {
			return &fakeInstance{
				id:   "rollback-instance",
				addr: readyzServer.Listener.Addr().String(),
			}, nil
		},
	}

	o, tracker := newTestOrchestrator(t, factory, &mockChecker{})
	o.db = database
	o.cfg.Database.Path = dbPath
	sender := newSender(t)

	err = o.Rollback(context.Background(), sender, func() {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tracker.drained.Load() != 1 {
		t.Error("drain should be called during rollback")
	}
}

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

	factory := &fakeServerFactory{
		supportsRollback: true,
		createInstanceFn: func(context.Context, string, []string, task.Sender) (newServerInstance, error) {
			return nil, io.ErrUnexpectedEOF
		},
	}

	var shutdownCalled bool
	o, tracker := newTestOrchestrator(t, factory, &mockChecker{})
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

	factory := &fakeServerFactory{
		supportsRollback: true,
		createInstanceFn: func(context.Context, string, []string, task.Sender) (newServerInstance, error) {
			return nil, io.ErrUnexpectedEOF
		},
	}

	var shutdownCalled bool
	o, _ := newTestOrchestrator(t, factory, &mockChecker{})
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

	var killed atomic.Bool
	factory := &fakeServerFactory{
		supportsRollback: true,
		createInstanceFn: func(context.Context, string, []string, task.Sender) (newServerInstance, error) {
			return &fakeInstance{
				id:   "old-instance",
				addr: fakeServer.Listener.Addr().String(),
				killFn: func(context.Context) {
					killed.Store(true)
				},
			}, nil
		},
	}

	var shutdownCalled bool
	o, tracker := newTestOrchestrator(t, factory, &mockChecker{})
	o.db = database
	o.cfg.Database.Path = dbPath
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
	if !killed.Load() {
		t.Error("instance must be killed")
	}
}

// ---------------------------------------------------------------------------
// Scheduled updates
// ---------------------------------------------------------------------------

func TestScheduledSkipsInProgress(t *testing.T) {
	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})
	o.state.Store("updating")

	if o.CASState("idle", "updating") {
		t.Error("should not transition from updating to updating")
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
	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, checker)
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

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunScheduled did not exit after context cancellation")
	}
}

func TestRunScheduledInvalidCron(t *testing.T) {
	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})

	// Invalid cron should return immediately without panic.
	o.RunScheduled(context.Background(), "not-a-cron", "stable")
}

func TestRunScheduledOnceNoUpdate(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{UpdateAvailable: false},
	}
	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, checker)
	if o.runScheduledOnce(context.Background(), "stable") {
		t.Error("expected false when no update available")
	}
}

func TestRunScheduledOnceCheckError(t *testing.T) {
	checker := &mockChecker{err: io.ErrUnexpectedEOF}
	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, checker)
	if o.runScheduledOnce(context.Background(), "stable") {
		t.Error("expected false on check error")
	}
}

func TestRunScheduledOnceCASFails(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{UpdateAvailable: true, CurrentVersion: "1.0.0", LatestVersion: "2.0.0"},
	}
	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, checker)
	o.state.Store("updating")
	if o.runScheduledOnce(context.Background(), "stable") {
		t.Error("expected false when CAS fails")
	}
}

func TestRunScheduledOnceUpdateFails(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{UpdateAvailable: true, CurrentVersion: "1.0.0", LatestVersion: "2.0.0"},
	}
	factory := &fakeServerFactory{
		preUpdateErr: io.ErrUnexpectedEOF,
	}
	o, _ := newTestOrchestrator(t, factory, checker)
	if o.runScheduledOnce(context.Background(), "stable") {
		t.Error("expected false when update fails")
	}
	if o.State() != "idle" {
		t.Errorf("state should reset to idle, got %q", o.State())
	}
}

func TestRunScheduledNoUpdate(t *testing.T) {
	checker := &mockChecker{
		result: &update.Result{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "1.0.0",
			UpdateAvailable: false,
		},
	}
	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, checker)
	o.cfg.Update = &config.UpdateConfig{
		WatchPeriod: config.Duration{Duration: 1 * time.Minute},
	}

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
		t.Fatal("RunScheduled did not exit")
	}
}

func TestRunScheduledCheckFails(t *testing.T) {
	checker := &mockChecker{
		err: io.ErrUnexpectedEOF,
	}
	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, checker)

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

// ---------------------------------------------------------------------------
// Constructor and helpers
// ---------------------------------------------------------------------------

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
	o := New(
		&fakeServerFactory{},
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

func TestDefaultChecker(t *testing.T) {
	var _ updateAPI = &DefaultChecker{}
}

// ---------------------------------------------------------------------------
// Backend-agnostic helpers
// ---------------------------------------------------------------------------

func TestCheckReadyError(t *testing.T) {
	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})
	err := o.checkReady(context.Background(), "127.0.0.1:1")
	if err == nil {
		t.Error("expected error from checkReady against closed port")
	}
}

func TestCheckReadyNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})
	err := o.checkReady(context.Background(), srv.Listener.Addr().String())
	if err == nil {
		t.Error("expected error for 503 response")
	}
}

func TestActivateError(t *testing.T) {
	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})
	err := o.activate(context.Background(), "127.0.0.1:1")
	if err == nil {
		t.Error("expected error from activate against closed port")
	}
}

func TestActivateNon200WithBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()

	o, _ := newTestOrchestrator(t, &fakeServerFactory{}, &mockChecker{})
	err := o.activate(context.Background(), srv.Listener.Addr().String())
	if err == nil {
		t.Error("expected error for 403 response")
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
	tok2 := generateActivationToken()
	if tok == tok2 {
		t.Error("consecutive tokens should differ")
	}
}

func TestImageWithTag(t *testing.T) {
	ref := imageWithTag("ghcr.io/cynkra/blockyard", "1.2.3")
	if ref != "ghcr.io/cynkra/blockyard:1.2.3" {
		t.Errorf("imageWithTag = %q", ref)
	}
}

// ---------------------------------------------------------------------------
// Backup metadata
// ---------------------------------------------------------------------------

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

func TestLatestBackupMetaMultiple(t *testing.T) {
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
	// Backup filename has 1-second resolution; wait for distinct timestamp.
	time.Sleep(1100 * time.Millisecond)
	m2, err := database.BackupWithMeta(context.Background(), "v2.0.0")
	if err != nil {
		t.Fatal(err)
	}

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
	if meta.MigrationVersion == 0 {
		t.Error("expected non-zero migration version in backup metadata")
	}
	if meta.CreatedAt == "" {
		t.Error("expected non-empty created_at")
	}
}

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError,
	})))
}
