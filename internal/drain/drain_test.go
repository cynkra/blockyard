package drain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
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
