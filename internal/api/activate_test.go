package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cynkra/blockyard/internal/auth"
)

func TestActivateEndpoint(t *testing.T) {
	srv := testServerForReadyz(t)
	srv.Passive.Store(true)

	activated := false
	startBG := func() { activated = true }

	handler := activateHandler(srv, startBG)

	adminCtx := auth.ContextWithCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleAdmin,
	})

	// First call: activates.
	r := httptest.NewRequest("POST", "/api/v1/admin/activate", nil).WithContext(adminCtx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !activated {
		t.Error("expected startBG to be called")
	}
	if srv.Passive.Load() {
		t.Error("expected Passive to be false after activation")
	}

	// Second call: conflict.
	r2 := httptest.NewRequest("POST", "/api/v1/admin/activate", nil).WithContext(adminCtx)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, r2)
	if w2.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d", w2.Code)
	}
}

func TestActivateWhenAlreadyActive(t *testing.T) {
	srv := testServerForReadyz(t)
	// Passive is false (default).

	handler := activateHandler(srv, func() {
		t.Error("startBG should not be called when not passive")
	})

	adminCtx := auth.ContextWithCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleAdmin,
	})
	r := httptest.NewRequest("POST", "/api/v1/admin/activate", nil).WithContext(adminCtx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d", w.Code)
	}
}

func TestActivateRequiresAdmin(t *testing.T) {
	srv := testServerForReadyz(t)
	srv.Passive.Store(true)

	handler := activateHandler(srv, func() {
		t.Error("startBG should not be called without admin auth")
	})

	// No auth context → nil caller → 403.
	r := httptest.NewRequest("POST", "/api/v1/admin/activate", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
	if !srv.Passive.Load() {
		t.Error("server should still be passive after rejected activation")
	}
}

// Verify the activate handler response has the expected JSON structure.
func TestActivateResponseJSON(t *testing.T) {
	srv := testServerForReadyz(t)
	srv.Passive.Store(true)

	handler := activateHandler(srv, func() {})

	adminCtx := auth.ContextWithCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleAdmin,
	})
	r := httptest.NewRequest("POST", "/api/v1/admin/activate", nil).WithContext(adminCtx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "activated" {
		t.Errorf("expected status 'activated', got %q", body["status"])
	}
}
