package drain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestDrainSetsFlag(t *testing.T) {
	srv := &server.Server{}
	d := &Drainer{Srv: srv}

	d.Drain()
	if !srv.Draining.Load() {
		t.Error("expected Draining to be true after Drain()")
	}
}

func TestUndrainClearsFlag(t *testing.T) {
	srv := &server.Server{}
	d := &Drainer{Srv: srv}

	d.Drain()
	d.Undrain()
	if srv.Draining.Load() {
		t.Error("expected Draining to be false after Undrain()")
	}
}

func TestFinishPreservesWorkers(t *testing.T) {
	be := mock.New()
	srv := server.NewServer(&config.Config{}, be, testDB(t))

	// Spawn a worker — Finish must leave it alive.
	be.Spawn(context.Background(), backend.WorkerSpec{WorkerID: "w1", AppID: "app1"}) //nolint:errcheck
	srv.Workers.Set("w1", server.ActiveWorker{AppID: "app1"})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()

	var wg sync.WaitGroup
	_, cancel := context.WithCancel(context.Background())

	d := &Drainer{
		Srv:        srv,
		MainServer: ts.Config,
		BGCancel:   cancel,
		BGWait:     &wg,
	}

	d.Drain()
	d.Finish(5 * time.Second)

	if !srv.Draining.Load() {
		t.Error("Draining flag should still be set after Finish")
	}
	if !be.HasWorker("w1") {
		t.Error("Finish must not evict workers (rolling update handoff)")
	}
}

func TestFinishWithMgmtServer(t *testing.T) {
	be := mock.New()
	srv := server.NewServer(&config.Config{}, be, testDB(t))

	mainTs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer mainTs.Close()

	mgmtTs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer mgmtTs.Close()

	var wg sync.WaitGroup
	_, cancel := context.WithCancel(context.Background())

	tracingCalled := false
	d := &Drainer{
		Srv:        srv,
		MainServer: mainTs.Config,
		MgmtServer: mgmtTs.Config,
		BGCancel:   cancel,
		BGWait:     &wg,
		TracingShutdown: func(ctx context.Context) error {
			tracingCalled = true
			return nil
		},
	}

	d.Finish(5 * time.Second)

	if !tracingCalled {
		t.Error("expected TracingShutdown to be called")
	}
}

func TestShutdownWithMgmtServerAndTracing(t *testing.T) {
	be := mock.New()
	srv := server.NewServer(&config.Config{}, be, testDB(t))

	mainTs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer mainTs.Close()

	mgmtTs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer mgmtTs.Close()

	var wg sync.WaitGroup
	_, cancel := context.WithCancel(context.Background())

	tracingCalled := false
	d := &Drainer{
		Srv:        srv,
		MainServer: mainTs.Config,
		MgmtServer: mgmtTs.Config,
		BGCancel:   cancel,
		BGWait:     &wg,
		TracingShutdown: func(ctx context.Context) error {
			tracingCalled = true
			return nil
		},
	}

	d.Shutdown(5 * time.Second)

	if !srv.Draining.Load() {
		t.Error("expected Draining=true")
	}
	if !tracingCalled {
		t.Error("expected TracingShutdown to be called")
	}
}

// fakeWorkerMap lets waitForIdle tests control the per-call return of
// WorkersForServer without spinning up a Redis. Only WorkersForServer
// is exercised by these tests; every other method delegates to an
// embedded MemoryWorkerMap so the type still satisfies the interface.
type fakeWorkerMap struct {
	*server.MemoryWorkerMap
	mu              sync.Mutex
	lastServerID    atomic.Value // string — last value passed to WorkersForServer
	workersByServer map[string][]string
}

func newFakeWorkerMap() *fakeWorkerMap {
	f := &fakeWorkerMap{
		MemoryWorkerMap: server.NewMemoryWorkerMap(),
		workersByServer: map[string][]string{},
	}
	f.lastServerID.Store("")
	return f
}

func (f *fakeWorkerMap) setWorkersForServer(serverID string, ids []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workersByServer[serverID] = append([]string(nil), ids...)
}

func (f *fakeWorkerMap) WorkersForServer(serverID string) []string {
	f.lastServerID.Store(serverID)
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.workersByServer[serverID]...)
}

// setIdleWaitPollIntervalForTest shortens waitForIdle's poll interval
// and returns a restore function. All waitForIdle tests call this so
// the 5-second default never blocks CI.
func setIdleWaitPollIntervalForTest(t *testing.T, d time.Duration) {
	t.Helper()
	prev := idleWaitPollInterval
	idleWaitPollInterval = d
	t.Cleanup(func() { idleWaitPollInterval = prev })
}

// drainerForIdleTest wires up a minimal Drainer whose Finish exercises
// waitForIdle end-to-end: real server state, real MainServer, no mgmt
// server, no tracing. The MainServer closes instantly; the interesting
// timing is in the idle-wait prelude.
func drainerForIdleTest(t *testing.T, srv *server.Server, finishIdleWait time.Duration, serverID string) *Drainer {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(ts.Close)

	var wg sync.WaitGroup
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return &Drainer{
		Srv:            srv,
		MainServer:     ts.Config,
		BGCancel:       cancel,
		BGWait:         &wg,
		FinishIdleWait: finishIdleWait,
		ServerID:       serverID,
	}
}

// TestFinishSkipsIdleWaitWhenDurationZero — FinishIdleWait=0 must skip
// the prelude entirely (Docker backend semantics, unchanged by phase
// 3-8). Regression guard against inadvertently polling when the field
// is unset.
func TestFinishSkipsIdleWaitWhenDurationZero(t *testing.T) {
	setIdleWaitPollIntervalForTest(t, 10*time.Millisecond)

	srv := server.NewServer(&config.Config{}, mock.New(), testDB(t))
	// One session on a worker so CountForWorkers would return >0 if
	// called — this would hang the test if waitForIdle ran.
	srv.Workers.Set("w1", server.ActiveWorker{AppID: "app1"})
	srv.Sessions.Set("sess-1", session.Entry{WorkerID: "w1"})

	d := drainerForIdleTest(t, srv, 0, "")

	start := time.Now()
	d.Finish(500 * time.Millisecond)
	elapsed := time.Since(start)

	// With FinishIdleWait=0 the prelude is skipped — Finish should
	// complete in well under the shutdown timeout and far under the
	// 5-second default poll interval.
	if elapsed > 250*time.Millisecond {
		t.Errorf("Finish with FinishIdleWait=0 took %v, expected near-instant", elapsed)
	}
}

// TestFinishWaitForIdleReturnsImmediatelyOnZeroSessions — happy path:
// the old server has no live sessions, Finish must not block on the
// poll loop even for one tick.
func TestFinishWaitForIdleReturnsImmediatelyOnZeroSessions(t *testing.T) {
	setIdleWaitPollIntervalForTest(t, 10*time.Millisecond)

	srv := server.NewServer(&config.Config{}, mock.New(), testDB(t))
	srv.Workers.Set("w1", server.ActiveWorker{AppID: "app1"})
	// No sessions set → CountForWorkers returns 0 → return before ticker.

	d := drainerForIdleTest(t, srv, time.Second, "test-server")

	start := time.Now()
	d.Finish(time.Second)
	elapsed := time.Since(start)

	// Zero-session short-circuit must not wait on the ticker at all.
	if elapsed >= 10*time.Millisecond*5 {
		t.Errorf("Finish waited %v for idle (expected immediate return)", elapsed)
	}
}

// TestFinishWaitForIdleDeadlineElapses — sessions remain active, the
// deadline must fire, and Finish must proceed (not hang forever).
// This is the safety-valve path: a misbehaving client that holds a
// session longer than the wait budget must not block cutover.
func TestFinishWaitForIdleDeadlineElapses(t *testing.T) {
	setIdleWaitPollIntervalForTest(t, 20*time.Millisecond)

	srv := server.NewServer(&config.Config{}, mock.New(), testDB(t))
	srv.Workers.Set("w1", server.ActiveWorker{AppID: "app1"})
	srv.Sessions.Set("sess-never-drains", session.Entry{WorkerID: "w1"})

	const idleWait = 80 * time.Millisecond
	d := drainerForIdleTest(t, srv, idleWait, "test-server")

	start := time.Now()
	d.Finish(time.Second)
	elapsed := time.Since(start)

	// Must wait at least the budget before giving up…
	if elapsed < idleWait {
		t.Errorf("Finish returned after %v, want >= %v (deadline honored)", elapsed, idleWait)
	}
	// …but must not spin forever waiting for a session that never ends.
	// Generous upper bound to absorb CI scheduling jitter.
	if elapsed > idleWait+500*time.Millisecond {
		t.Errorf("Finish took %v with an active session, expected to give up near %v", elapsed, idleWait)
	}
}

// TestFinishWaitForIdleSessionDrainsMidWait — a session being cleared
// while the poll loop is running must unblock Finish promptly on the
// next tick. Verifies the cold-path return (sessions=0 after a tick,
// not at the very first check).
func TestFinishWaitForIdleSessionDrainsMidWait(t *testing.T) {
	setIdleWaitPollIntervalForTest(t, 15*time.Millisecond)

	srv := server.NewServer(&config.Config{}, mock.New(), testDB(t))
	srv.Workers.Set("w1", server.ActiveWorker{AppID: "app1"})
	srv.Sessions.Set("sess-clearing", session.Entry{WorkerID: "w1"})

	// Clear the session after ~50ms — a few polls in, well before
	// the 1s deadline.
	go func() {
		time.Sleep(50 * time.Millisecond)
		srv.Sessions.Delete("sess-clearing")
	}()

	d := drainerForIdleTest(t, srv, time.Second, "test-server")

	start := time.Now()
	d.Finish(time.Second)
	elapsed := time.Since(start)

	// Must have observed a non-zero count at least once (waited
	// past the 50ms clearing point).
	if elapsed < 50*time.Millisecond {
		t.Errorf("Finish returned in %v — earlier than the session-clear goroutine", elapsed)
	}
	// Must have picked up the drain reasonably quickly after the
	// clear — a few poll intervals of slack, not the full deadline.
	if elapsed > 300*time.Millisecond {
		t.Errorf("Finish took %v after session cleared, expected near-immediate pickup", elapsed)
	}
}

// TestFinishWaitForIdleFiltersByServerID — load-bearing regression
// guard for the same-host rolling-update fix. The drainer must pass
// its ServerID through to WorkersForServer so it counts only its own
// sessions, not a new peer's fresh sessions.
//
// Setup: two "workers" in the map but only one is owned by the
// draining server. The peer's worker has sessions, the draining
// server's own worker has none. Finish must return on the happy
// path, not hang waiting for the peer's sessions.
func TestFinishWaitForIdleFiltersByServerID(t *testing.T) {
	setIdleWaitPollIntervalForTest(t, 10*time.Millisecond)

	const (
		oldServerID = "old-server"
		newServerID = "new-server"
		oldWorker   = "old-worker"
		newWorker   = "new-worker"
	)

	fake := newFakeWorkerMap()
	fake.setWorkersForServer(oldServerID, nil)                   // no workers → no sessions
	fake.setWorkersForServer(newServerID, []string{newWorker})   // peer has a worker

	srv := server.NewServer(&config.Config{}, mock.New(), testDB(t))
	srv.Workers = fake
	// The peer's worker has an active session; without the
	// ServerID filter, Finish would wait for it.
	srv.Sessions.Set("peer-sess", session.Entry{WorkerID: newWorker})

	d := drainerForIdleTest(t, srv, 500*time.Millisecond, oldServerID)

	start := time.Now()
	d.Finish(time.Second)
	elapsed := time.Since(start)

	// Because waitForIdle filtered to oldServerID's workers (empty),
	// the poll loop must short-circuit on the first iteration.
	if elapsed >= 150*time.Millisecond {
		t.Errorf("Finish waited %v — ServerID filter not honored (peer sessions bled through)", elapsed)
	}
	if got := fake.lastServerID.Load().(string); got != oldServerID {
		t.Errorf("WorkersForServer called with %q, want %q", got, oldServerID)
	}
}

func TestShutdownEvictsWorkers(t *testing.T) {
	be := mock.New()
	srv := server.NewServer(&config.Config{}, be, testDB(t))

	// Spawn workers — Shutdown must evict them.
	be.Spawn(context.Background(), backend.WorkerSpec{WorkerID: "w1", AppID: "app1"}) //nolint:errcheck
	srv.Workers.Set("w1", server.ActiveWorker{AppID: "app1"})
	be.Spawn(context.Background(), backend.WorkerSpec{WorkerID: "w2", AppID: "app2"}) //nolint:errcheck
	srv.Workers.Set("w2", server.ActiveWorker{AppID: "app2"})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()

	var wg sync.WaitGroup
	_, cancel := context.WithCancel(context.Background())

	d := &Drainer{
		Srv:        srv,
		MainServer: ts.Config,
		BGCancel:   cancel,
		BGWait:     &wg,
	}

	d.Shutdown(5 * time.Second)

	if !srv.Draining.Load() {
		t.Error("Draining flag should be set after Shutdown")
	}
	if be.HasWorker("w1") || be.HasWorker("w2") {
		t.Error("Shutdown must evict all workers")
	}
	if len(srv.Workers.All()) != 0 {
		t.Errorf("expected 0 workers in map, got %d", len(srv.Workers.All()))
	}
}
