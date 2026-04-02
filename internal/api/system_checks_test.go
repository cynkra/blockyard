package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/preflight"
	"github.com/cynkra/blockyard/internal/server"
)

func setupCheckerServer(t *testing.T) *server.Server {
	t.Helper()
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	srv := server.NewServer(&config.Config{}, mock.New(), database)
	srv.Checker = preflight.NewChecker(preflight.RuntimeDeps{
		DBPing:     func(ctx context.Context) error { return nil },
		DockerPing: func(ctx context.Context) error { return nil },
	})
	srv.Checker.Init(context.Background(), &preflight.Report{}, nil)
	return srv
}

func TestGetSystemChecks_NilLatest(t *testing.T) {
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	srv := server.NewServer(&config.Config{}, mock.New(), database)
	// Checker with no Init call — Latest() returns nil.
	srv.Checker = preflight.NewChecker(preflight.RuntimeDeps{})

	handler := GetSystemChecks(srv)
	req := httptest.NewRequest("GET", "/api/v1/system/checks", nil)
	ctx := auth.ContextWithCaller(req.Context(), &auth.CallerIdentity{
		Sub:  "admin",
		Role: auth.RoleAdmin,
	})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req.WithContext(ctx))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var report preflight.Report
	if err := json.NewDecoder(w.Body).Decode(&report); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if report.Results == nil {
		// JSON decodes null array as nil, but empty array decodes as [].
		// Either is acceptable — just verify the response is valid JSON.
	}
	if report.Summary.Errors != 0 || report.Summary.Warnings != 0 {
		t.Errorf("expected empty summary, got %+v", report.Summary)
	}
}

func TestGetSystemChecks_Forbidden(t *testing.T) {
	srv := setupCheckerServer(t)
	handler := GetSystemChecks(srv)

	// No caller in context.
	req := httptest.NewRequest("GET", "/api/v1/system/checks", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}

	// Publisher, not admin.
	req = httptest.NewRequest("GET", "/api/v1/system/checks", nil)
	ctx := auth.ContextWithCaller(req.Context(), &auth.CallerIdentity{
		Sub:  "user",
		Role: auth.RolePublisher,
	})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req.WithContext(ctx))
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for publisher, got %d", w.Code)
	}
}

func TestGetSystemChecks_AdminOK(t *testing.T) {
	srv := setupCheckerServer(t)
	handler := GetSystemChecks(srv)

	req := httptest.NewRequest("GET", "/api/v1/system/checks", nil)
	ctx := auth.ContextWithCaller(req.Context(), &auth.CallerIdentity{
		Sub:  "admin",
		Role: auth.RoleAdmin,
	})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req.WithContext(ctx))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var report preflight.Report
	if err := json.NewDecoder(w.Body).Decode(&report); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(report.Results) == 0 {
		t.Error("expected results in report")
	}
}

func TestRunSystemChecks_Forbidden(t *testing.T) {
	srv := setupCheckerServer(t)
	handler := RunSystemChecks(srv)

	req := httptest.NewRequest("POST", "/api/v1/system/checks/run", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestRunSystemChecks_AdminOK(t *testing.T) {
	srv := setupCheckerServer(t)
	handler := RunSystemChecks(srv)

	req := httptest.NewRequest("POST", "/api/v1/system/checks/run", nil)
	ctx := auth.ContextWithCaller(req.Context(), &auth.CallerIdentity{
		Sub:  "admin",
		Role: auth.RoleAdmin,
	})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req.WithContext(ctx))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var report preflight.Report
	if err := json.NewDecoder(w.Body).Decode(&report); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
}
