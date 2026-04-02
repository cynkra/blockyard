package drain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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

func TestFinishShutdownsServers(t *testing.T) {
	srv := server.NewServer(&config.Config{}, mock.New(), testDB(t))

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
}
