package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scopedPolicy is a policy with per-user identity templating.
const scopedPolicy = `path "secret/data/users/{{identity.entity.aliases.auth_jwt_1234.name}}/*" { capabilities = ["read"] }`

// unscopedPolicy is a policy missing identity templating.
const unscopedPolicy = `path "secret/data/*" { capabilities = ["read"] }`

// fullMockBao creates a mock that passes all bootstrap checks,
// including a properly scoped policy.
func fullMockBao(t *testing.T) *Client {
	return fullMockBaoWithPolicy(t, scopedPolicy)
}

func fullMockBaoWithPolicy(t *testing.T, policy string) *Client {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jwt/": map[string]any{"type": "jwt"},
		})
	})

	mux.HandleFunc("GET /v1/auth/jwt/role/blockyard-user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"role_type":       "jwt",
				"token_policies":  []string{"default", "blockyard-user"},
			},
		})
	})

	mux.HandleFunc("GET /v1/sys/mounts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"secret/": map[string]any{"type": "kv", "options": map[string]any{"version": "2"}},
		})
	})

	mux.HandleFunc("GET /v1/sys/policies/acl/blockyard-user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"policy": policy},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))
}

func TestBootstrap_AllPass(t *testing.T) {
	client := fullMockBao(t)
	if err := Bootstrap(context.Background(), client, "jwt", false); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
}

func TestBootstrap_HealthFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error when health fails")
	}
	if !strings.Contains(err.Error(), "health") {
		t.Errorf("expected 'health' in error, got %v", err)
	}
}

func TestBootstrap_JWTAuthMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"token/": map[string]any{"type": "token"},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error when JWT auth missing")
	}
	if !strings.Contains(err.Error(), "JWT auth method not enabled") {
		t.Errorf("expected 'JWT auth method not enabled' in error, got %v", err)
	}
}

func TestBootstrap_RoleMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jwt/": map[string]any{"type": "jwt"},
		})
	})
	mux.HandleFunc("GET /v1/auth/jwt/role/blockyard-user", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error when role missing")
	}
	if !strings.Contains(err.Error(), "blockyard-user role not found") {
		t.Errorf("expected 'blockyard-user role not found' in error, got %v", err)
	}
}

func TestCheckPolicyScoping_Scoped(t *testing.T) {
	// Should succeed — policy contains identity templating.
	client := fullMockBao(t)
	if err := checkPolicyScoping(context.Background(), client, "jwt"); err != nil {
		t.Fatalf("expected no error for scoped policy, got: %v", err)
	}
}

func TestCheckPolicyScoping_Unscoped(t *testing.T) {
	// Should return error — policy is missing identity templating.
	client := fullMockBaoWithPolicy(t, unscopedPolicy)
	err := checkPolicyScoping(context.Background(), client, "jwt")
	if err == nil {
		t.Fatal("expected error for unscoped policy")
	}
	if !strings.Contains(err.Error(), "no attached policy uses per-user path scoping") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCheckPolicyScoping_PolicyReadFails(t *testing.T) {
	// Role returns policies but the policy endpoint 404s.
	// Should return error since no scoped policy was found.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/auth/jwt/role/blockyard-user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"token_policies": []string{"nonexistent-policy"},
			},
		})
	})
	mux.HandleFunc("GET /v1/sys/policies/acl/nonexistent-policy", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := checkPolicyScoping(context.Background(), client, "jwt")
	if err == nil {
		t.Fatal("expected error when policy read fails")
	}
}

func TestBootstrap_KVMountMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jwt/": map[string]any{"type": "jwt"},
		})
	})
	mux.HandleFunc("GET /v1/auth/jwt/role/blockyard-user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	})
	mux.HandleFunc("GET /v1/sys/mounts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"sys/": map[string]any{"type": "system"},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error when KV mount missing")
	}
	if !strings.Contains(err.Error(), "KV v2 secrets engine not mounted") {
		t.Errorf("expected 'KV v2 secrets engine not mounted' in error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Error/failure path tests for malformed and unexpected responses
// ---------------------------------------------------------------------------

func TestBootstrap_JWTAuthMalformedJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{not valid json`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error for malformed JWT auth response")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected 'decode' in error, got %v", err)
	}
}

func TestBootstrap_JWTAuthMissingTypeField(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		// Valid JSON but missing the "jwt/" key entirely.
		json.NewEncoder(w).Encode(map[string]any{
			"other/": map[string]any{"something": "else"},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error when JWT auth key is missing from response")
	}
	if !strings.Contains(err.Error(), "JWT auth method not enabled") {
		t.Errorf("expected 'JWT auth method not enabled' in error, got %v", err)
	}
}

func TestBootstrap_RoleMalformedJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jwt/": map[string]any{"type": "jwt"},
		})
	})
	mux.HandleFunc("GET /v1/auth/jwt/role/blockyard-user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{not valid json`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error for malformed role response")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected 'decode' in error, got %v", err)
	}
}

func TestBootstrap_RoleUnexpectedStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jwt/": map[string]any{"type": "jwt"},
		})
	})
	mux.HandleFunc("GET /v1/auth/jwt/role/blockyard-user", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error for unexpected role status")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("expected 'status 500' in error, got %v", err)
	}
}

func TestBootstrap_KVMountMalformedJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jwt/": map[string]any{"type": "jwt"},
		})
	})
	mux.HandleFunc("GET /v1/auth/jwt/role/blockyard-user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	})
	mux.HandleFunc("GET /v1/sys/mounts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`not json at all`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error for malformed mounts response")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected 'decode' in error, got %v", err)
	}
}

func TestBootstrap_KVMountUnexpectedStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jwt/": map[string]any{"type": "jwt"},
		})
	})
	mux.HandleFunc("GET /v1/auth/jwt/role/blockyard-user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	})
	mux.HandleFunc("GET /v1/sys/mounts", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error for forbidden mounts status")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected '403' in error, got %v", err)
	}
}

func TestBootstrap_PolicyMalformedJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jwt/": map[string]any{"type": "jwt"},
		})
	})
	mux.HandleFunc("GET /v1/auth/jwt/role/blockyard-user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"token_policies": []string{"my-policy"},
			},
		})
	})
	mux.HandleFunc("GET /v1/sys/mounts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"secret/": map[string]any{"type": "kv", "options": map[string]any{"version": "2"}},
		})
	})
	mux.HandleFunc("GET /v1/sys/policies/acl/my-policy", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{{{malformed`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	// Policy read failure is logged as a warning and skipped, so the
	// error should be about no scoped policy found (not a decode error).
	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error when policy is malformed")
	}
	if !strings.Contains(err.Error(), "no attached policy uses per-user path scoping") {
		t.Errorf("expected scoping error, got %v", err)
	}
}

func TestBootstrap_PolicyEmptyBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jwt/": map[string]any{"type": "jwt"},
		})
	})
	mux.HandleFunc("GET /v1/auth/jwt/role/blockyard-user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"token_policies": []string{"empty-policy"},
			},
		})
	})
	mux.HandleFunc("GET /v1/sys/mounts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"secret/": map[string]any{"type": "kv", "options": map[string]any{"version": "2"}},
		})
	})
	mux.HandleFunc("GET /v1/sys/policies/acl/empty-policy", func(w http.ResponseWriter, r *http.Request) {
		// Valid JSON but both data.policy and rules are empty.
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"policy": ""},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error when policy body is empty")
	}
	if !strings.Contains(err.Error(), "no attached policy uses per-user path scoping") {
		t.Errorf("expected scoping error, got %v", err)
	}
}

func TestBootstrap_JWTAuthUnexpectedStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err == nil {
		t.Fatal("expected error for forbidden auth status")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected '403' in error, got %v", err)
	}
}

func TestBootstrap_PolicyFallbackToRules(t *testing.T) {
	// Test readPolicy's fallback path: data.policy is empty, uses rules field.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sys/auth", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jwt/": map[string]any{"type": "jwt"},
		})
	})
	mux.HandleFunc("GET /v1/auth/jwt/role/blockyard-user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"token_policies": []string{"legacy-policy"},
			},
		})
	})
	mux.HandleFunc("GET /v1/sys/mounts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"secret/": map[string]any{"type": "kv", "options": map[string]any{"version": "2"}},
		})
	})
	mux.HandleFunc("GET /v1/sys/policies/acl/legacy-policy", func(w http.ResponseWriter, r *http.Request) {
		// Use the v1 API format: rules field at top level, no data.policy.
		json.NewEncoder(w).Encode(map[string]any{
			"rules": `path "secret/data/users/{{identity.entity.aliases.auth_jwt_1234.name}}/*" { capabilities = ["read"] }`,
			"data":  map[string]any{"policy": ""},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, StaticAdmin(func() string { return "admin-token" }))

	err := Bootstrap(context.Background(), client, "jwt", false)
	if err != nil {
		t.Fatalf("expected no error for scoped policy via rules field, got: %v", err)
	}
}
