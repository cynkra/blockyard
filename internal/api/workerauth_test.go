package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
)

func testWorkerSigningKey() *auth.SigningKey {
	return auth.NewSigningKey([]byte("test-worker-token-key-32-bytes!!"))
}

func makeWorkerToken(t *testing.T, key *auth.SigningKey, sub, app, wid string, ttl time.Duration) string {
	t.Helper()
	now := time.Now()
	claims := &auth.SessionTokenClaims{
		Sub: sub,
		App: app,
		Wid: wid,
		Iat: now.Unix(),
		Exp: now.Add(ttl).Unix(),
	}
	token, err := auth.EncodeSessionToken(claims, key)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestWorkerAuth_ValidToken(t *testing.T) {
	key := testWorkerSigningKey()
	token := makeWorkerToken(t, key, "worker:w1", "app-1", "w1", 15*time.Minute)

	handler := WorkerAuth(key)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wid := WorkerIDFromContext(r.Context())
		appID := AppIDFromContext(r.Context())
		if wid != "w1" {
			t.Errorf("WorkerID = %q, want %q", wid, "w1")
		}
		if appID != "app-1" {
			t.Errorf("AppID = %q, want %q", appID, "app-1")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/api/v1/packages", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestWorkerAuth_MissingToken(t *testing.T) {
	key := testWorkerSigningKey()
	handler := WorkerAuth(key)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/api/v1/packages", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestWorkerAuth_ExpiredToken(t *testing.T) {
	key := testWorkerSigningKey()
	token := makeWorkerToken(t, key, "worker:w1", "app-1", "w1", -5*time.Minute)

	handler := WorkerAuth(key)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/api/v1/packages", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestWorkerAuth_UserTokenRejected(t *testing.T) {
	key := testWorkerSigningKey()
	// Token without "worker:" prefix in Sub.
	token := makeWorkerToken(t, key, "user:u1", "app-1", "w1", 15*time.Minute)

	handler := WorkerAuth(key)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/api/v1/packages", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}
