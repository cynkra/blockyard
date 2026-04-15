package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockBao creates a minimal OpenBao mock server for unit tests.
func mockBao(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, StaticAdmin(func() string { return "test-admin-token" }))
}

func TestHealth_OK(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sys/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("expected healthy, got %v", err)
	}
}

func TestHealth_Sealed(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	err := client.Health(context.Background())
	if err == nil {
		t.Fatal("expected error for sealed vault")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("expected 503 in error, got %v", err)
	}
}

func TestHealth_Standby(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))

	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("standby should be considered healthy, got %v", err)
	}
}

func TestJWTLogin_Success(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/jwt/login" {
			http.NotFound(w, r)
			return
		}
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["role"] != "blockyard-user" {
			http.Error(w, "bad role", http.StatusBadRequest)
			return
		}
		if body["jwt"] == "" {
			http.Error(w, "missing jwt", http.StatusBadRequest)
			return
		}

		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "s.test-token-123",
				"lease_duration": 3600,
			},
		})
	}))

	token, ttl, err := client.JWTLogin(context.Background(), "jwt", "my-access-token")
	if err != nil {
		t.Fatalf("JWTLogin: %v", err)
	}
	if token != "s.test-token-123" {
		t.Errorf("token = %q, want s.test-token-123", token)
	}
	if ttl.Seconds() != 3600 {
		t.Errorf("ttl = %v, want 3600s", ttl)
	}
}

func TestJWTLogin_InvalidJWT(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"errors":["invalid jwt"]}`))
	}))

	_, _, err := client.JWTLogin(context.Background(), "jwt", "bad-token")
	if err == nil {
		t.Fatal("expected error for invalid JWT")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("expected 400 in error, got %v", err)
	}
}

func TestJWTLogin_EmptyClientToken(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "",
				"lease_duration": 0,
			},
		})
	}))

	_, _, err := client.JWTLogin(context.Background(), "jwt", "some-token")
	if err == nil {
		t.Fatal("expected error for empty client_token")
	}
}

func TestKVWrite_Success(t *testing.T) {
	var receivedPath string
	var receivedToken string
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedToken = r.Header.Get("X-Vault-Token")
		w.WriteHeader(http.StatusOK)
	}))

	err := client.KVWrite(context.Background(), "users/test-sub/apikeys/openai", map[string]any{
		"api_key": "sk-test",
	})
	if err != nil {
		t.Fatalf("KVWrite: %v", err)
	}
	if receivedPath != "/v1/secret/data/users/test-sub/apikeys/openai" {
		t.Errorf("path = %q, want /v1/secret/data/users/test-sub/apikeys/openai", receivedPath)
	}
	if receivedToken != "test-admin-token" {
		t.Errorf("token = %q, want test-admin-token", receivedToken)
	}
}

func TestKVWrite_Error(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))

	err := client.KVWrite(context.Background(), "some/path", map[string]any{"key": "val"})
	if err == nil {
		t.Fatal("expected error for forbidden write")
	}
}

func TestKVRead_Success(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") == "" {
			http.Error(w, "missing token", http.StatusForbidden)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"data": map[string]any{
					"api_key": "sk-secret",
				},
			},
		})
	}))

	data, err := client.KVRead(context.Background(), "users/sub/apikeys/svc", "user-token")
	if err != nil {
		t.Fatalf("KVRead: %v", err)
	}
	if data["api_key"] != "sk-secret" {
		t.Errorf("api_key = %v, want sk-secret", data["api_key"])
	}
}

func TestKVRead_NotFound(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	_, err := client.KVRead(context.Background(), "no/such/path", "token")
	if err == nil {
		t.Fatal("expected error for not found")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %v", err)
	}
}

func TestKVReadAdmin_RetriesOn403(t *testing.T) {
	// First admin request returns 403; after a Reauth the second
	// request returns 200. We assert the caller sees the 200 payload
	// and that exactly one login + two KV reads happened.
	var loginCount, readCount int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/approle/login", func(w http.ResponseWriter, r *http.Request) {
		loginCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "tok-v" + string(rune('0'+loginCount)),
				"lease_duration": 300,
			},
		})
	})
	mux.HandleFunc("GET /v1/secret/data/foo", func(w http.ResponseWriter, r *http.Request) {
		readCount++
		if readCount == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		// After reauth the new token must be attached.
		if r.Header.Get("X-Vault-Token") != "tok-v2" {
			t.Errorf("retry used wrong token: %q", r.Header.Get("X-Vault-Token"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"data": map[string]any{"api_key": "revealed"},
			},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	auth := NewAppRoleAuth(srv.URL, AppRoleCreds{RoleIDEnv: "r", SecretIDEnv: "s"})
	if err := auth.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	client := NewClient(srv.URL, auth)

	data, err := client.KVReadAdmin(context.Background(), "foo")
	if err != nil {
		t.Fatalf("KVReadAdmin: %v", err)
	}
	if data["api_key"] != "revealed" {
		t.Errorf("api_key = %v", data["api_key"])
	}
	if loginCount != 2 {
		t.Errorf("login count = %d, want 2 (initial + reauth)", loginCount)
	}
	if readCount != 2 {
		t.Errorf("read count = %d, want 2 (initial 403 + retry)", readCount)
	}
}

func TestKVReadAdmin_HardFailOn403Twice(t *testing.T) {
	// Vault always returns 403. After one reauth attempt the retry
	// also 403s — we expect a hard error rather than an infinite
	// retry loop.
	var readCount int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/approle/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "tok",
				"lease_duration": 300,
			},
		})
	})
	mux.HandleFunc("GET /v1/secret/data/foo", func(w http.ResponseWriter, r *http.Request) {
		readCount++
		w.WriteHeader(http.StatusForbidden)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	auth := NewAppRoleAuth(srv.URL, AppRoleCreds{RoleIDEnv: "r", SecretIDEnv: "s"})
	if err := auth.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	client := NewClient(srv.URL, auth)

	_, err := client.KVReadAdmin(context.Background(), "foo")
	if err == nil {
		t.Fatal("expected error after persistent 403")
	}
	if readCount != 2 {
		t.Errorf("read count = %d, want 2 (one retry only, no loop)", readCount)
	}
}
