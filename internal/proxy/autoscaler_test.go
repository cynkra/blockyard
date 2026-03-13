package proxy

import (
	"context"
	"testing"
	"time"

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

func TestAutoscaleKeepsLastWorker(t *testing.T) {
	srv := testAutoscaleServer(t)
	app := createTestApp(t, srv, "my-app", true)

	maxSessions := 5
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		MaxSessionsPerWorker: &maxSessions,
	})
	app, _ = srv.DB.GetApp(app.ID)

	// Single worker with 0 sessions — should NOT be evicted (keep at least 1).
	_, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	autoscaleTick(context.Background(), srv)

	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) != 1 {
		t.Errorf("expected 1 worker (keep last), got %d", len(workerIDs))
	}
}
