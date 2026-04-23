package integration

import (
	"context"
	"encoding/json"
	"fmt"
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
	return NewClient(srv.URL, func() string { return "test-admin-token" })
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

// sysAuthFlatBody mirrors vault's real /v1/sys/auth response: mount
// entries keyed with a trailing slash, mixed with top-level metadata
// fields that are strings, numbers, nulls, and objects. Covers the
// decoder regression from #285 where a strictly-typed map decoded
// straight from the body failed on the metadata fields.
const sysAuthFlatBody = `{
  "request_id": "abc-123",
  "lease_id": "",
  "lease_duration": 0,
  "renewable": false,
  "wrap_info": null,
  "warnings": null,
  "auth": null,
  "jwt/": {
    "accessor": "auth_jwt_abc123",
    "type": "jwt",
    "description": ""
  },
  "token/": {
    "accessor": "auth_token_def456",
    "type": "token"
  }
}`

// sysAuthNestedBody is the alternate shape where auth methods live
// under "data". The decoder must fall back to that on miss.
const sysAuthNestedBody = `{
  "request_id": "xyz-789",
  "data": {
    "jwt/": {"accessor": "auth_jwt_nested", "type": "jwt"}
  }
}`

func TestAuthMountAccessor_FlatShape(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sys/auth" || r.Method != "GET" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(sysAuthFlatBody))
	}))

	got, err := client.AuthMountAccessor(context.Background(), "jwt")
	if err != nil {
		t.Fatalf("AuthMountAccessor: %v", err)
	}
	if got != "auth_jwt_abc123" {
		t.Errorf("accessor = %q, want %q", got, "auth_jwt_abc123")
	}

	// Caller may pass the path with or without the trailing slash.
	got2, err := client.AuthMountAccessor(context.Background(), "jwt/")
	if err != nil {
		t.Fatalf("AuthMountAccessor (trailing slash): %v", err)
	}
	if got2 != "auth_jwt_abc123" {
		t.Errorf("accessor = %q with trailing slash", got2)
	}
}

func TestAuthMountAccessor_NestedShape(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sysAuthNestedBody))
	}))

	got, err := client.AuthMountAccessor(context.Background(), "jwt")
	if err != nil {
		t.Fatalf("AuthMountAccessor: %v", err)
	}
	if got != "auth_jwt_nested" {
		t.Errorf("accessor = %q, want %q", got, "auth_jwt_nested")
	}
}

func TestAuthMountAccessor_MissingMount(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sysAuthFlatBody))
	}))

	_, err := client.AuthMountAccessor(context.Background(), "oidc")
	if err == nil {
		t.Fatal("expected error for missing auth method")
	}
	if !strings.Contains(err.Error(), "no auth method") {
		t.Errorf("expected 'no auth method' in error, got %v", err)
	}
}

func TestAuthMountAccessor_EmptyAccessor(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"jwt/": {"type": "jwt"}}`)
	}))

	_, err := client.AuthMountAccessor(context.Background(), "jwt")
	if err == nil {
		t.Fatal("expected error for empty accessor")
	}
}

func TestIdentityLookupEntityByAlias_Success(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/identity/lookup/entity" || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req["alias_name"] != "alice" {
			t.Errorf("alias_name = %q, want alice", req["alias_name"])
		}
		if req["alias_mount_accessor"] != "auth_jwt_abc123" {
			t.Errorf("alias_mount_accessor = %q", req["alias_mount_accessor"])
		}
		fmt.Fprint(w, `{"data":{"id":"entity-uuid-1"}}`)
	}))

	got, err := client.IdentityLookupEntityByAlias(context.Background(),
		"alice", "auth_jwt_abc123")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "entity-uuid-1" {
		t.Errorf("entity id = %q", got)
	}
}

func TestIdentityLookupEntityByAlias_UnknownAlias(t *testing.T) {
	client := mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Vault answers unknown aliases with 204 No Content.
		w.WriteHeader(http.StatusNoContent)
	}))

	_, err := client.IdentityLookupEntityByAlias(context.Background(),
		"nobody", "auth_jwt_abc123")
	if err == nil {
		t.Fatal("expected error for unknown alias")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %v", err)
	}
}
