package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAppRoleLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/approle/login" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if r.Method != "POST" {
			t.Errorf("unexpected method: %s", r.Method)
			w.WriteHeader(405)
			return
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["role_id"] != "test-role" || body["secret_id"] != "test-secret" {
			w.WriteHeader(400)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "hvs.approle-token",
				"lease_duration": 3600,
			},
		})
	}))
	defer srv.Close()

	token, ttl, err := AppRoleLogin(context.Background(), srv.Client(), srv.URL, "test-role", "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if token != "hvs.approle-token" {
		t.Errorf("token = %q", token)
	}
	if ttl != 1*time.Hour {
		t.Errorf("ttl = %v", ttl)
	}
}

func TestAppRoleLoginError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()

	_, _, err := AppRoleLogin(context.Background(), srv.Client(), srv.URL, "bad", "bad")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRenewSelf(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/token/renew-self" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("X-Vault-Token") != "hvs.my-token" {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "hvs.my-token",
				"lease_duration": 7200,
			},
		})
	}))
	defer srv.Close()

	ttl, err := RenewSelf(context.Background(), srv.Client(), srv.URL, "hvs.my-token")
	if err != nil {
		t.Fatal(err)
	}
	if ttl != 2*time.Hour {
		t.Errorf("ttl = %v", ttl)
	}
}

func TestRenewSelfError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()

	_, err := RenewSelf(context.Background(), srv.Client(), srv.URL, "bad")
	if err == nil {
		t.Fatal("expected error")
	}
}
