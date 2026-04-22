package proxy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
)

func testAutoscaleServer(t *testing.T) *server.Server {
	t.Helper()
	srv := testColdstartServer(t)
	srv.Config.Proxy.HealthInterval = config.Duration{Duration: 100 * time.Millisecond}
	return srv
}

func setSession(srv *server.Server, id, workerID string) {
	srv.Sessions.Set(id, session.Entry{WorkerID: workerID, LastAccess: time.Now()})
}

func TestAutoscaleScaleUp(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Update app to allow multiple sessions and workers.
	maxSessions := 2
	maxWorkers := 3
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		MaxSessionsPerWorker: &maxSessions,
		MaxWorkersPerApp:     &maxWorkers,
	})

	// Spawn initial worker and fill it to capacity.
	wid, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	setSession(srv, "s1", wid)
	setSession(srv, "s2", wid)

	// Re-fetch app (updated max_sessions).
	app, _ = srv.DB.GetApp(app.ID)

	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(workerIDs))
	}

	// Run autoscale tick — should spawn a new worker.
	autoscaleTick(context.Background(), srv)

	workerIDs = srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 2 {
		t.Errorf("expected 2 workers after scale-up, got %d", len(workerIDs))
	}
}

func TestAutoscaleScaleDown(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	maxSessions := 5
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		MaxSessionsPerWorker: &maxSessions,
	})
	app, _ = srv.DB.GetApp(app.ID)

	// Spawn two workers, only one has sessions.
	wid1, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	wid2, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	setSession(srv, "s1", wid1)
	// wid2 has 0 sessions — mark as idle long enough to be reaped.
	srv.Workers.SetIdleSince(wid2, time.Now().Add(-10*time.Minute))
	// Set a short idle worker timeout so the test triggers eviction.
	srv.Config.Proxy.IdleWorkerTimeout.Duration = 1 * time.Second

	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 2 {
		t.Fatalf("expected 2 workers, got %d", len(workerIDs))
	}

	autoscaleTick(context.Background(), srv)

	workerIDs = srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 1 {
		t.Errorf("expected 1 worker after scale-down, got %d", len(workerIDs))
	}

	// The remaining worker should be the one with sessions.
	_, ok := srv.Workers.Get(wid1)
	if !ok {
		t.Error("expected worker with sessions to survive")
	}
	_, ok = srv.Workers.Get(wid2)
	if ok {
		t.Error("expected idle worker to be evicted")
	}
}

func TestAutoscaleRespectsPerAppLimit(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	maxSessions := 1
	maxWorkers := 1
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		MaxSessionsPerWorker: &maxSessions,
		MaxWorkersPerApp:     &maxWorkers,
	})
	app, _ = srv.DB.GetApp(app.ID)

	wid, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	setSession(srv, "s1", wid) // at capacity

	autoscaleTick(context.Background(), srv)

	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 1 {
		t.Errorf("expected 1 worker (at per-app limit), got %d", len(workerIDs))
	}
}

func TestAutoscaleRespectsGlobalLimit(t *testing.T) {
	srv := testAutoscaleServer(t)
	srv.Config.Proxy.MaxWorkers = 1

	app := createTestApp(t, srv, "my-app", true)

	maxSessions := 1
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		MaxSessionsPerWorker: &maxSessions,
	})
	app, _ = srv.DB.GetApp(app.ID)

	wid, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	setSession(srv, "s1", wid) // at capacity

	autoscaleTick(context.Background(), srv)

	if srv.Workers.Count() != 1 {
		t.Errorf("expected 1 worker (at global limit), got %d", srv.Workers.Count())
	}
}

func TestAutoscaleSkipsDrainingApps(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	maxSessions := 1
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		MaxSessionsPerWorker: &maxSessions,
	})
	app, _ = srv.DB.GetApp(app.ID)

	wid, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	setSession(srv, "s1", wid)

	// Mark as draining — autoscaler should skip.
	srv.Workers.MarkDraining(app.ID)

	autoscaleTick(context.Background(), srv)

	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 1 {
		t.Errorf("expected 1 worker (draining, skipped), got %d", len(workerIDs))
	}
}

func TestAutoscaleScaleToZero(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	maxSessions := 5
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		MaxSessionsPerWorker: &maxSessions,
	})
	app, _ = srv.DB.GetApp(app.ID)

	// Single worker with 0 sessions — should be evicted (scale to zero).
	_, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	// Set a short idle worker timeout so the test triggers eviction.
	srv.Config.Proxy.IdleWorkerTimeout.Duration = 0

	autoscaleTick(context.Background(), srv)

	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 0 {
		t.Errorf("expected 0 workers (scale to zero), got %d", len(workerIDs))
	}
}

func TestRunAutoscalerStopsOnContextCancel(t *testing.T) {
	srv := testAutoscaleServer(t)
	srv.Config.Proxy.HealthInterval = config.Duration{Duration: 50 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		RunAutoscaler(ctx, srv)
		close(done)
	}()

	// Let the autoscaler run for a few ticks.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// RunAutoscaler returned — success.
	case <-time.After(2 * time.Second):
		t.Fatal("RunAutoscaler did not return after context cancel")
	}
}

func TestAutoscaleEvictsUnhealthy(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Spawn 2 workers while backend reports healthy.
	wid1, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	wid2, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 2 {
		t.Fatalf("expected 2 workers, got %d", len(workerIDs))
	}

	// Mark backend as unhealthy — both workers should be evicted.
	be := srv.Backend.(*mock.MockBackend)
	be.HealthOK.Store(false)

	autoscaleTick(context.Background(), srv)

	if _, ok := srv.Workers.Get(wid1); ok {
		t.Error("expected worker 1 to be evicted")
	}
	if _, ok := srv.Workers.Get(wid2); ok {
		t.Error("expected worker 2 to be evicted")
	}
}

func TestAutoscaleSessionSweep(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Set a very short session idle TTL.
	srv.Config.Proxy.SessionIdleTTL = config.Duration{Duration: 50 * time.Millisecond}

	// Spawn a worker and create a session with an old LastAccess.
	wid, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	srv.Sessions.Set("old-sess", session.Entry{
		WorkerID:   wid,
		LastAccess: time.Now().Add(-10 * time.Minute),
	})

	if _, ok := srv.Sessions.Get("old-sess"); !ok {
		t.Fatal("session should exist before sweep")
	}

	autoscaleTick(context.Background(), srv)

	if _, ok := srv.Sessions.Get("old-sess"); ok {
		t.Error("expected idle session to be swept")
	}
}

func TestAutoscaleSkipsMissingApp(t *testing.T) {
	srv := testAutoscaleServer(t)

	// Register workers for an app ID that doesn't exist in the DB.
	srv.Workers.Set("orphan-w1", server.ActiveWorker{AppID: "nonexistent-app"})
	srv.Workers.Set("orphan-w2", server.ActiveWorker{AppID: "nonexistent-app"})

	// Should not panic — the tick handles missing apps gracefully.
	// The workers have zero sessions and idle timeout is 0 in test config,
	// so they are marked idle and evicted. This is correct: orphan workers
	// for deleted apps should be cleaned up.
	autoscaleTick(context.Background(), srv)

	if srv.Workers.Count() != 0 {
		t.Errorf("expected orphan workers to be evicted, got %d remaining", srv.Workers.Count())
	}
}

func TestEvictUnhealthyReturnsHealthy(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)
	be := srv.Backend.(*mock.MockBackend)

	// Spawn 3 workers while healthy.
	wid1, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	wid2, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	wid3, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	// Backdate StartedAt past WorkerStartTimeout so these are treated
	// as post-cold-start — evictUnhealthy's cold-start exemption does
	// not apply.
	backdateStart(srv, time.Hour, wid1, wid2, wid3)

	// All unhealthy — should evict all.
	be.HealthOK.Store(false)
	healthy := evictUnhealthy(context.Background(), srv, []string{wid1, wid2, wid3})
	if len(healthy) != 0 {
		t.Errorf("expected 0 healthy workers, got %d", len(healthy))
	}

	// Spawn 2 new workers while healthy.
	be.HealthOK.Store(true)
	wid4, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	wid5, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	healthy = evictUnhealthy(context.Background(), srv, []string{wid4, wid5})
	if len(healthy) != 2 {
		t.Errorf("expected 2 healthy workers, got %d", len(healthy))
	}
}

// Regression for #269: when HealthInterval is shorter than a worker's
// real cold-start time, the autoscaler tick must not evict a still-
// starting worker as "crashed" — spawnWorker's pollHealthy owns that
// window.
func TestEvictUnhealthySkipsColdStart(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)
	be := srv.Backend.(*mock.MockBackend)

	wid, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	// Worker hasn't finished its cold-start window yet and isn't
	// answering health checks.
	be.HealthOK.Store(false)

	healthy := evictUnhealthy(context.Background(), srv, []string{wid})
	if len(healthy) != 1 {
		t.Fatalf("cold-starting worker must be kept, got %d retained", len(healthy))
	}
	if _, ok := srv.Workers.Get(wid); !ok {
		t.Error("cold-starting worker was evicted from the worker map")
	}

	// Once the cold-start window elapses, the same unhealthy worker
	// is correctly treated as crashed.
	backdateStart(srv, time.Hour, wid)
	healthy = evictUnhealthy(context.Background(), srv, []string{wid})
	if len(healthy) != 0 {
		t.Errorf("post-cold-start unhealthy worker must be evicted, got %d retained", len(healthy))
	}
	if _, ok := srv.Workers.Get(wid); ok {
		t.Error("post-cold-start unhealthy worker was not evicted")
	}
}

func backdateStart(srv *server.Server, d time.Duration, workerIDs ...string) {
	for _, wid := range workerIDs {
		w, ok := srv.Workers.Get(wid)
		if !ok {
			continue
		}
		w.StartedAt = time.Now().Add(-d)
		srv.Workers.Set(wid, w)
	}
}

func TestEnsurePreWarmedSpawnsWorkers(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	sessions := 2
	srv.DB.UpdateApp(app.ID, db.AppUpdate{PreWarmedSessions: &sessions})
	app, _ = srv.DB.GetApp(app.ID)

	ensurePreWarmed(context.Background(), srv, app)

	// Default max_sessions_per_worker = 1, so 2 pre-warmed sessions → 2 workers.
	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 2 {
		t.Errorf("expected 2 pre-warmed workers, got %d", len(workerIDs))
	}
}

func TestEnsurePreWarmedPoolAlreadyFull(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	sessions := 1
	srv.DB.UpdateApp(app.ID, db.AppUpdate{PreWarmedSessions: &sessions})
	app, _ = srv.DB.GetApp(app.ID)

	// Spawn one idle worker — pool is already full.
	spawnWorker(context.Background(), srv, app)

	before := srv.Workers.Count()
	ensurePreWarmed(context.Background(), srv, app)

	if srv.Workers.Count() != before {
		t.Errorf("expected no new workers (pool full), got %d total", srv.Workers.Count())
	}
}

func TestEnsurePreWarmedRespectsPerAppLimit(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	sessions := 3
	maxWorkers := 2
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		PreWarmedSessions: &sessions,
		MaxWorkersPerApp:  &maxWorkers,
	})
	app, _ = srv.DB.GetApp(app.ID)

	ensurePreWarmed(context.Background(), srv, app)

	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 2 {
		t.Errorf("expected 2 workers (capped by per-app limit), got %d", len(workerIDs))
	}
}

func TestEnsurePreWarmedRespectsGlobalLimit(t *testing.T) {
	srv := testAutoscaleServer(t)
	srv.Config.Proxy.MaxWorkers = 1

	app := createTestApp(t, srv, "my-app", true)

	sessions := 3
	srv.DB.UpdateApp(app.ID, db.AppUpdate{PreWarmedSessions: &sessions})
	app, _ = srv.DB.GetApp(app.ID)

	ensurePreWarmed(context.Background(), srv, app)

	if srv.Workers.Count() != 1 {
		t.Errorf("expected 1 worker (global limit), got %d", srv.Workers.Count())
	}
}

func TestEnsurePreWarmedZeroNoop(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// pre_warmed_sessions = 0 (default) — should be a no-op.
	ensurePreWarmed(context.Background(), srv, app)

	if srv.Workers.Count() != 0 {
		t.Errorf("expected 0 workers, got %d", srv.Workers.Count())
	}
}

// TestEnsurePreWarmedMultiSessionWorker verifies that with max_sessions > 1,
// a single idle worker satisfies a pre_warmed_sessions target up to
// max_sessions_per_worker. This is the core behavior change from issue #160:
// the unit is now free session slots, not idle workers.
func TestEnsurePreWarmedMultiSessionWorker(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	sessions := 5
	maxSessions := 5
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		PreWarmedSessions:    &sessions,
		MaxSessionsPerWorker: &maxSessions,
	})
	app, _ = srv.DB.GetApp(app.ID)

	ensurePreWarmed(context.Background(), srv, app)

	// One worker exposes 5 free slots → target satisfied.
	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 1 {
		t.Errorf("expected 1 worker (5 free slots covers target), got %d", len(workerIDs))
	}
}

// TestEnsurePreWarmedMultiSessionPartialConsumption verifies that once a slot
// is consumed on the only idle worker, a second worker is spawned to restore
// the target (overshooting by max_sessions - 1 slots).
func TestEnsurePreWarmedMultiSessionPartialConsumption(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	sessions := 5
	maxSessions := 5
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		PreWarmedSessions:    &sessions,
		MaxSessionsPerWorker: &maxSessions,
	})
	app, _ = srv.DB.GetApp(app.ID)

	// Spawn one worker and consume one slot.
	wid, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	setSession(srv, "s1", wid)

	// freeSlots = 5-1 = 4 < 5 → spawn another worker.
	ensurePreWarmed(context.Background(), srv, app)

	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 2 {
		t.Errorf("expected 2 workers (replenishing free slots), got %d", len(workerIDs))
	}
}

// TestEnsurePreWarmedMultiSessionRoundsUp verifies that when the target is
// larger than max_sessions_per_worker, spawn count rounds up to a whole
// worker boundary (ceil(target / max_sessions)).
func TestEnsurePreWarmedMultiSessionRoundsUp(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	sessions := 7
	maxSessions := 3
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		PreWarmedSessions:    &sessions,
		MaxSessionsPerWorker: &maxSessions,
	})
	app, _ = srv.DB.GetApp(app.ID)

	ensurePreWarmed(context.Background(), srv, app)

	// ceil(7/3) = 3 workers, providing 9 free slots.
	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 3 {
		t.Errorf("expected 3 workers (ceil(7/3)), got %d", len(workerIDs))
	}
}

func TestPreWarmAppsFromDB(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	sessions := 1
	srv.DB.UpdateApp(app.ID, db.AppUpdate{PreWarmedSessions: &sessions})

	// preWarmApps queries the DB — should find the app and spawn.
	preWarmApps(context.Background(), srv)

	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 1 {
		t.Errorf("expected 1 pre-warmed worker from DB query, got %d", len(workerIDs))
	}
}

func TestAutoscalePreWarmAfterEviction(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	sessions := 1
	srv.DB.UpdateApp(app.ID, db.AppUpdate{PreWarmedSessions: &sessions})
	app, _ = srv.DB.GetApp(app.ID)

	// Spawn a worker, then make it idle and evict it.
	wid, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	_ = wid

	// Set idle timeout to 0 so the worker gets evicted.
	srv.Config.Proxy.IdleWorkerTimeout.Duration = 0

	// Run autoscale tick — should evict idle worker then pre-warm a new one.
	autoscaleTick(context.Background(), srv)

	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 1 {
		t.Errorf("expected 1 worker (pre-warmed after eviction), got %d", len(workerIDs))
	}
}

func TestEnsurePreWarmedClaimedWorkerReplacement(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	sessions := 1
	srv.DB.UpdateApp(app.ID, db.AppUpdate{PreWarmedSessions: &sessions})
	app, _ = srv.DB.GetApp(app.ID)

	// Spawn one idle worker (the warm pool).
	wid, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate claiming: add session and clear idle.
	setSession(srv, "s1", wid)
	srv.Workers.ClearIdleSince(wid)

	// Now the pool has a deficit — ensurePreWarmed should spawn a replacement.
	ensurePreWarmed(context.Background(), srv, app)

	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 2 {
		t.Errorf("expected 2 workers (original + replacement), got %d", len(workerIDs))
	}
}

func TestPreWarmAppsSkipsDisabled(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	sessions := 2
	srv.DB.UpdateApp(app.ID, db.AppUpdate{PreWarmedSessions: &sessions})
	srv.DB.SetAppEnabled(app.ID, false)

	before := srv.Workers.Count()
	preWarmApps(context.Background(), srv)

	if srv.Workers.Count() != before {
		t.Errorf("expected no new workers for disabled app, got %d total", srv.Workers.Count())
	}
}

// TestPreWarmAppsRunsDuringDrain confirms the new semantics: when an
// app's existing workers are draining (e.g. after a redeploy), the
// pre-warmer still spawns fresh workers for the new bundle. Under the
// old "any worker draining = app draining" rule this was blocked.
func TestPreWarmAppsRunsDuringDrain(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	sessions := 2
	srv.DB.UpdateApp(app.ID, db.AppUpdate{PreWarmedSessions: &sessions})

	srv.Workers.Set("drain-w", server.ActiveWorker{AppID: app.ID})
	srv.Workers.MarkDraining(app.ID)

	before := srv.Workers.Count()
	preWarmApps(context.Background(), srv)

	if srv.Workers.Count() <= before {
		t.Errorf("expected pre-warm to spawn new workers alongside the drained one, got %d total", srv.Workers.Count())
	}
}

func TestPreWarmAppsNoPreWarmedApps(t *testing.T) {
	srv := testAutoscaleServer(t)
	// Create an app with 0 pre-warmed sessions (default).
	createTestApp(t, srv, "no-warm-app", true)

	// Should be a no-op — must not panic.
	preWarmApps(context.Background(), srv)

	if srv.Workers.Count() != 0 {
		t.Errorf("expected 0 workers, got %d", srv.Workers.Count())
	}
}

func TestPreWarmAppsMultipleApps(t *testing.T) {
	srv := testAutoscaleServer(t)
	app1 := createTestApp(t, srv, "app-a", true)
	app2 := createTestApp(t, srv, "app-b", true)

	sessions := 1
	srv.DB.UpdateApp(app1.ID, db.AppUpdate{PreWarmedSessions: &sessions})
	srv.DB.UpdateApp(app2.ID, db.AppUpdate{PreWarmedSessions: &sessions})

	preWarmApps(context.Background(), srv)

	if srv.Workers.CountForApp(app1.ID) != 1 {
		t.Errorf("expected 1 worker for app-a, got %d", srv.Workers.CountForApp(app1.ID))
	}
	if srv.Workers.CountForApp(app2.ID) != 1 {
		t.Errorf("expected 1 worker for app-b, got %d", srv.Workers.CountForApp(app2.ID))
	}
}

func TestTryScaleUpFailure(t *testing.T) {
	fb := &faultyBackend{
		MockBackend: mock.New(),
		spawnErr:    fmt.Errorf("container runtime unavailable"),
	}
	srv := testColdstartServerWithBackend(t, fb)
	srv.Config.Proxy.HealthInterval = config.Duration{Duration: 100 * time.Millisecond}

	app := createTestApp(t, srv, "my-app", true)

	// Set max sessions to 1 so the single worker is at capacity.
	maxSessions := 1
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		MaxSessionsPerWorker: &maxSessions,
	})
	app, _ = srv.DB.GetApp(app.ID)

	// Manually register a worker + session to simulate capacity.
	srv.Workers.Set("existing-w", server.ActiveWorker{AppID: app.ID})
	srv.Registry.Set("existing-w", "127.0.0.1:9999")
	setSession(srv, "s1", "existing-w")

	// tryScaleUp should fail (faulty backend) but not panic.
	tryScaleUp(context.Background(), srv, app, []string{"existing-w"})

	// Only the original worker should remain.
	if srv.Workers.Count() != 1 {
		t.Errorf("expected 1 worker, got %d", srv.Workers.Count())
	}
}
