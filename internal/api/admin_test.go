package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/orchestrator"
)

func TestAdminUpdateRequiresAdmin(t *testing.T) {
	srv := testServerForReadyz(t)
	// Use a real orchestrator so the nil check doesn't mask the auth check.
	orch := orchestrator.NewForTest()
	handler := handleAdminUpdate(srv, orch)

	// No auth context → nil caller → 403.
	r := httptest.NewRequest("POST", "/api/v1/admin/update", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestAdminUpdateNativeMode(t *testing.T) {
	srv := testServerForReadyz(t)
	// nil orchestrator → native mode.
	handler := handleAdminUpdate(srv, nil)

	adminCtx := auth.ContextWithCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleAdmin,
	})
	r := httptest.NewRequest("POST", "/api/v1/admin/update", nil).WithContext(adminCtx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("expected 501, got %d", w.Code)
	}
}

func TestAdminRollbackRequiresAdmin(t *testing.T) {
	srv := testServerForReadyz(t)
	orch := orchestrator.NewForTest()
	handler := handleAdminRollback(srv, orch)

	r := httptest.NewRequest("POST", "/api/v1/admin/rollback", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestAdminRollbackNativeMode(t *testing.T) {
	srv := testServerForReadyz(t)
	handler := handleAdminRollback(srv, nil)

	adminCtx := auth.ContextWithCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleAdmin,
	})
	r := httptest.NewRequest("POST", "/api/v1/admin/rollback", nil).WithContext(adminCtx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("expected 501, got %d", w.Code)
	}
}

func TestAdminStatusIdle(t *testing.T) {
	// nil orchestrator → idle state.
	handler := handleAdminUpdateStatus(nil)

	r := httptest.NewRequest("GET", "/api/v1/admin/update/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["state"] != "idle" {
		t.Errorf("expected state idle, got %q", body["state"])
	}
}

func TestAdminStatusNonIdle(t *testing.T) {
	orch := orchestrator.NewForTest()
	orch.SetState("watching")
	handler := handleAdminUpdateStatus(orch)

	r := httptest.NewRequest("GET", "/api/v1/admin/update/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	var body map[string]string
	json.NewDecoder(w.Body).Decode(&body)
	if body["state"] != "watching" {
		t.Errorf("expected state watching, got %q", body["state"])
	}
}

func TestAdminUpdateChannelOverride(t *testing.T) {
	srv := testServerForReadyz(t)
	orch := orchestrator.NewForTest()
	handler := handleAdminUpdate(srv, orch)

	adminCtx := auth.ContextWithCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleAdmin,
	})
	body := strings.NewReader(`{"channel":"main"}`)
	r := httptest.NewRequest("POST", "/api/v1/admin/update", body).WithContext(adminCtx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
}

func TestActivateWithNoTokenNoAuth(t *testing.T) {
	// No activation token env var, no admin auth → should be rejected.
	t.Setenv("BLOCKYARD_ACTIVATION_TOKEN", "")
	srv := testServerForReadyz(t)
	srv.Passive.Store(true)

	handler := activateHandler(srv, func() {
		t.Error("startBG should not be called")
	})

	r := httptest.NewRequest("POST", "/api/v1/admin/activate", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestActivateWithActivationToken(t *testing.T) {
	// Set the activation token env var.
	t.Setenv("BLOCKYARD_ACTIVATION_TOKEN", "test-secret-token")

	srv := testServerForReadyz(t)
	srv.Passive.Store(true)

	activated := false
	handler := activateHandler(srv, func() { activated = true })

	// Request with matching token — no admin context needed.
	r := httptest.NewRequest("POST", "/api/v1/admin/activate", nil)
	r.Header.Set("Authorization", "Bearer test-secret-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !activated {
		t.Error("expected startBG to be called with activation token")
	}
}

func TestActivateWithWrongToken(t *testing.T) {
	t.Setenv("BLOCKYARD_ACTIVATION_TOKEN", "test-secret-token")

	srv := testServerForReadyz(t)
	srv.Passive.Store(true)

	handler := activateHandler(srv, func() {
		t.Error("startBG should not be called with wrong token")
	})

	r := httptest.NewRequest("POST", "/api/v1/admin/activate", nil)
	r.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestAdminUpdateReturnsTaskID(t *testing.T) {
	srv := testServerForReadyz(t)
	orch := orchestrator.NewForTest()
	handler := handleAdminUpdate(srv, orch)

	adminCtx := auth.ContextWithCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleAdmin,
	})
	r := httptest.NewRequest("POST", "/api/v1/admin/update", nil).WithContext(adminCtx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["task_id"] == "" {
		t.Error("expected non-empty task_id")
	}
}

func TestAdminRollbackReturnsTaskID(t *testing.T) {
	srv := testServerForReadyz(t)
	orch := orchestrator.NewForTest()
	handler := handleAdminRollback(srv, orch)

	adminCtx := auth.ContextWithCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleAdmin,
	})
	r := httptest.NewRequest("POST", "/api/v1/admin/rollback", nil).WithContext(adminCtx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["task_id"] == "" {
		t.Error("expected non-empty task_id")
	}
}

// TestAdminUpdateConflict verifies that a second update returns 409
// when the orchestrator is already updating. We use a minimal
// orchestrator for this since we only need the state machine.
func TestAdminUpdateConflict(t *testing.T) {
	srv := testServerForReadyz(t)

	// Create a minimal orchestrator just for state tracking.
	orch := orchestrator.NewForTest()
	orch.SetState("updating")

	handler := handleAdminUpdate(srv, orch)

	adminCtx := auth.ContextWithCaller(context.Background(), &auth.CallerIdentity{
		Role: auth.RoleAdmin,
	})
	r := httptest.NewRequest("POST", "/api/v1/admin/update", nil).WithContext(adminCtx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}
