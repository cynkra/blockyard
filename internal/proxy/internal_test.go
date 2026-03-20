package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

func TestHandleBlockyardInternalReady(t *testing.T) {
	srv := testColdstartServer(t)
	app := &db.AppRow{ID: "app-1", Name: "my-app"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/app/my-app/__blockyard/ready", nil)

	handled := handleBlockyardInternal(rec, req, app, "my-app", srv)
	if !handled {
		t.Fatal("expected handleBlockyardInternal to return true")
	}

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var body readyResponse
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Ready {
		t.Error("expected ready=false when no workers exist")
	}
}

func TestHandleBlockyardInternalNormalPath(t *testing.T) {
	srv := testColdstartServer(t)
	app := &db.AppRow{ID: "app-1", Name: "my-app"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/app/my-app/some/path", nil)

	handled := handleBlockyardInternal(rec, req, app, "my-app", srv)
	if handled {
		t.Fatal("expected handleBlockyardInternal to return false for normal path")
	}
}

func TestHandleBlockyardInternalUnknown404(t *testing.T) {
	srv := testColdstartServer(t)
	app := &db.AppRow{ID: "app-1", Name: "my-app"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/app/my-app/__blockyard/unknown", nil)

	handled := handleBlockyardInternal(rec, req, app, "my-app", srv)
	if !handled {
		t.Fatal("expected handleBlockyardInternal to return true")
	}

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandleReadyWithHealthyWorker(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Spawn a healthy worker.
	wid, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}
	_ = wid

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/app/my-app/__blockyard/ready", nil)

	handleReady(rec, req, app, srv)

	var body readyResponse
	json.NewDecoder(rec.Body).Decode(&body)
	if !body.Ready {
		t.Error("expected ready=true when healthy worker exists")
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("expected no-store, got %q", cc)
	}
}

func TestHandleReadyWithUnhealthyWorker(t *testing.T) {
	srv := testColdstartServer(t)
	be := srv.Backend.(*mock.MockBackend)

	app := createTestApp(t, srv, "my-app", true)

	// Spawn a worker while healthy.
	_, _, err := spawnWorker(context.Background(), srv, app)
	if err != nil {
		t.Fatal(err)
	}

	// Mark backend as unhealthy.
	be.HealthOK.Store(false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/app/my-app/__blockyard/ready", nil)

	handleReady(rec, req, app, srv)

	var body readyResponse
	json.NewDecoder(rec.Body).Decode(&body)
	if body.Ready {
		t.Error("expected ready=false when worker is unhealthy")
	}
}

func TestHandleReadyNoWorkers(t *testing.T) {
	srv := testColdstartServer(t)
	app := &db.AppRow{ID: "app-1", Name: "my-app"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/app/my-app/__blockyard/ready", nil)

	handleReady(rec, req, app, srv)

	var body readyResponse
	json.NewDecoder(rec.Body).Decode(&body)
	if body.Ready {
		t.Error("expected ready=false when no workers exist")
	}
}

func TestHandleReadySkipsDraining(t *testing.T) {
	srv := testColdstartServer(t)
	app := createTestApp(t, srv, "my-app", true)

	// Spawn and mark draining.
	be := srv.Backend.(*mock.MockBackend)
	be.Spawn(context.Background(), backend.WorkerSpec{WorkerID: "w1"})
	srv.Workers.Set("w1", server.ActiveWorker{AppID: app.ID, Draining: true})
	srv.Registry.Set("w1", "127.0.0.1:1234")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/app/my-app/__blockyard/ready", nil)

	handleReady(rec, req, app, srv)

	var body readyResponse
	json.NewDecoder(rec.Body).Decode(&body)
	if body.Ready {
		t.Error("expected ready=false when only draining workers exist")
	}
}
