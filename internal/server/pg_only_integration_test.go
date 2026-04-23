package server

import (
	"context"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/cynkra/blockyard/internal/registry"
	"github.com/cynkra/blockyard/internal/session"
)

// Postgres-only end-to-end integration for #262. No Redis at all.
// Exercises the three postgres-primary stores together so the shared
// blockyard_workers table (registry + workermap) and the cross-store
// worker/session linkage are actually hit as a set. This is the
// Postgres-only DoD bullet: "Postgres-only mode (no Redis configured)
// works correctly."
//
// Skips when BLOCKYARD_TEST_POSTGRES_URL is not set. CI's `unit` job
// always provides one.

type pgOnlyStack struct {
	db       *sqlx.DB
	sessions *session.PostgresStore
	registry *registry.PostgresRegistry
	workers  *PostgresWorkerMap
}

func newPGOnlyStack(t *testing.T) *pgOnlyStack {
	t.Helper()
	db := testPGDB(t)
	return &pgOnlyStack{
		db:       db,
		sessions: session.NewPostgresStore(db, time.Hour),
		registry: registry.NewPostgresRegistry(db, time.Minute),
		workers:  NewPostgresWorkerMap(db, "server-1"),
	}
}

// TestPGOnly_StickySessionLifecycle walks a realistic sticky-session
// flow without Redis: spawn a worker, create a session routed to it,
// touch, drain, delete, tear down. Pins that the three stores
// compose correctly on top of the shared blockyard_workers table.
func TestPGOnly_StickySessionLifecycle(t *testing.T) {
	s := newPGOnlyStack(t)

	// ── Spawn: workermap first (full row), then registry (address). In
	// production cmd/blockyard writes workermap first and registry
	// second; both must upsert onto the same row without clobbering
	// each other's columns.
	s.workers.Set("w1", ActiveWorker{
		AppID:     "app1",
		BundleID:  "b1",
		StartedAt: time.Now(),
	})
	s.registry.Set("w1", "127.0.0.1:3838")

	// Both views must now be coherent.
	w, ok := s.workers.Get("w1")
	if !ok || w.AppID != "app1" {
		t.Fatalf("workers.Get = (%+v, %v), want app1/true", w, ok)
	}
	addr, ok := s.registry.Get("w1")
	if !ok || addr != "127.0.0.1:3838" {
		t.Fatalf("registry.Get = (%q, %v), want (127.0.0.1:3838, true)", addr, ok)
	}

	// ── Route a session to the worker.
	s.sessions.Set("sess-1", session.Entry{
		WorkerID:   "w1",
		UserSub:    "user-a",
		LastAccess: time.Now(),
	})

	entry, ok := s.sessions.Get("sess-1")
	if !ok || entry.WorkerID != "w1" {
		t.Fatalf("sessions.Get = (%+v, %v), want w1/true", entry, ok)
	}

	// Subsequent requests for the same session must keep the routing
	// via Touch (the hot-path operation).
	if !s.sessions.Touch("sess-1") {
		t.Error("Touch on existing session should return true")
	}

	// ── Drain: mark the worker draining. The registry entry must stay
	// so in-flight sessions can still reach it; only new-session
	// routing would skip it.
	s.workers.SetDraining("w1")
	w, _ = s.workers.Get("w1")
	if !w.Draining {
		t.Error("worker should report draining after SetDraining")
	}
	if _, ok := s.registry.Get("w1"); !ok {
		t.Error("registry must still resolve a draining worker")
	}

	// ── Session ends. Session row goes away; worker stays until torn
	// down by the health loop.
	s.sessions.Delete("sess-1")
	if _, ok := s.sessions.Get("sess-1"); ok {
		t.Error("sessions.Get should miss after Delete")
	}

	// ── Teardown: workermap and registry both clear the blockyard_workers
	// row. Either order is valid; the last Delete wins.
	s.registry.Delete("w1")
	s.workers.Delete("w1")
	if _, ok := s.workers.Get("w1"); ok {
		t.Error("worker should be gone after teardown")
	}
	if _, ok := s.registry.Get("w1"); ok {
		t.Error("registry should be empty after teardown")
	}
}

// TestPGOnly_RegistryHeartbeatExpiry pins the TTL contract in a
// full-stack context: if the health poller stops refreshing a worker,
// registry.Get starts returning false after registryTTL, and the
// workermap still knows the row exists (because heartbeat expiry is
// a registry concern, not a workermap one).
func TestPGOnly_RegistryHeartbeatExpiry(t *testing.T) {
	s := newPGOnlyStack(t)

	s.workers.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})
	s.registry.Set("w1", "127.0.0.1:3838")

	// Simulate a dropped heartbeat: push last_heartbeat past the TTL.
	if _, err := s.db.Exec(
		`UPDATE blockyard_workers SET last_heartbeat = now() - interval '2 minutes' WHERE id = $1`,
		"w1",
	); err != nil {
		t.Fatal(err)
	}

	if _, ok := s.registry.Get("w1"); ok {
		t.Error("registry should treat stale-heartbeat worker as gone")
	}
	// workermap has no TTL of its own — the row is still discoverable
	// until something explicitly deletes it.
	if _, ok := s.workers.Get("w1"); !ok {
		t.Error("workermap should still see the row (TTL is registry-only)")
	}
}

// TestPGOnly_SessionExpirySweep pins the DoD bullet that "sessions that
// are created and then never touched again" get swept by the
// PostgresStore background expiry, independent of Redis.
func TestPGOnly_SessionExpirySweep(t *testing.T) {
	db := testPGDB(t)
	// Short idleTTL so expires_at lands in the past after a Sleep.
	s := session.NewPostgresStore(db, 10*time.Millisecond)

	s.Set("sess-old", session.Entry{WorkerID: "w1", LastAccess: time.Now()})

	// Run the expiry loop briefly; it deletes past-due rows.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.RunExpiry(ctx, 20*time.Millisecond)
		close(done)
	}()
	<-done

	if _, ok := s.Get("sess-old"); ok {
		t.Error("expired session should be gone after RunExpiry")
	}
}

// TestPGOnly_RerouteAfterWorkerReplacement mirrors a rolling update:
// sessions on w1 get rerouted onto w2, and the session store reports
// the new worker for subsequent lookups. Runs end-to-end against
// Postgres primaries for all three stores.
func TestPGOnly_RerouteAfterWorkerReplacement(t *testing.T) {
	s := newPGOnlyStack(t)

	// Two workers for the same app — simulates a rolling update in
	// progress.
	s.workers.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})
	s.registry.Set("w1", "127.0.0.1:3838")
	s.workers.Set("w2", ActiveWorker{AppID: "app1", BundleID: "b2", StartedAt: time.Now()})
	s.registry.Set("w2", "127.0.0.1:3839")

	s.sessions.Set("sess-1", session.Entry{WorkerID: "w1", LastAccess: time.Now()})
	s.sessions.Set("sess-2", session.Entry{WorkerID: "w1", LastAccess: time.Now()})

	// Reroute both sessions off w1 onto w2.
	if n := s.sessions.RerouteWorker("w1", "w2"); n != 2 {
		t.Errorf("RerouteWorker = %d, want 2", n)
	}

	for _, id := range []string{"sess-1", "sess-2"} {
		e, ok := s.sessions.Get(id)
		if !ok {
			t.Errorf("%s should still exist", id)
			continue
		}
		if e.WorkerID != "w2" {
			t.Errorf("%s worker = %q, want w2", id, e.WorkerID)
		}
	}
	if n := s.sessions.CountForWorker("w1"); n != 0 {
		t.Errorf("CountForWorker(w1) = %d, want 0", n)
	}
	if n := s.sessions.CountForWorker("w2"); n != 2 {
		t.Errorf("CountForWorker(w2) = %d, want 2", n)
	}
}
