package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// approleServer returns a test vault that answers POST /v1/auth/approle/login.
// It records the received secret_id for each call and returns a fresh
// token on every call (so callers can tell apart multiple logins).
func approleServer(t *testing.T, leaseSeconds int) (url string, seen *[]string) {
	t.Helper()
	var (
		mu     sync.Mutex
		tokens atomic.Int32
		got    []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/approle/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			RoleID   string `json:"role_id"`
			SecretID string `json:"secret_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, body.SecretID)
		mu.Unlock()
		n := tokens.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   fmt.Sprintf("hvs.token-%d", n),
				"lease_duration": leaseSeconds,
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &got
}

func TestAppRoleAuthLoginFromEnv(t *testing.T) {
	url, seen := approleServer(t, 3600)
	t.Setenv("BLOCKYARD_VAULT_SECRET_ID", "env-secret")

	a := NewAppRoleAuth(url, "my-role", "")
	if err := a.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.Token(); got != "hvs.token-1" {
		t.Errorf("Token() = %q, want hvs.token-1", got)
	}
	if !a.Healthy() {
		t.Error("Healthy() = false, want true after successful login")
	}
	if len(*seen) != 1 || (*seen)[0] != "env-secret" {
		t.Errorf("server received secret_ids %v, want [env-secret]", *seen)
	}
}

func TestAppRoleAuthReloginPicksUpRotatedFile(t *testing.T) {
	url, seen := approleServer(t, 3600)
	dir := t.TempDir()
	path := filepath.Join(dir, "secret_id")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAppRoleAuth(url, "my-role", path)
	if err := a.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	token1 := a.Token()

	// Rotate the file on disk.
	if err := os.WriteFile(path, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	token2 := a.Token()

	if token1 == token2 {
		t.Errorf("expected a fresh token after re-login, got %q twice", token1)
	}
	if len(*seen) != 2 || (*seen)[0] != "first" || (*seen)[1] != "second" {
		t.Errorf("server received %v, want [first second]", *seen)
	}
}

func TestAppRoleAuthLoginErrors(t *testing.T) {
	url, _ := approleServer(t, 3600)

	t.Run("no env, no file", func(t *testing.T) {
		// Make sure env is unset for this subtest.
		t.Setenv("BLOCKYARD_VAULT_SECRET_ID", "")
		os.Unsetenv("BLOCKYARD_VAULT_SECRET_ID")
		a := NewAppRoleAuth(url, "my-role", "")
		if err := a.Login(context.Background()); err == nil {
			t.Error("expected error when neither env nor file is set")
		}
		if a.Healthy() {
			t.Error("Healthy() = true after failed login")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "secret_id")
		os.WriteFile(path, []byte(""), 0o600)
		a := NewAppRoleAuth(url, "my-role", path)
		if err := a.Login(context.Background()); err == nil {
			t.Error("expected error for empty secret_id file")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		a := NewAppRoleAuth(url, "my-role", "/nonexistent/path/to/secret_id")
		if err := a.Login(context.Background()); err == nil {
			t.Error("expected error for missing secret_id file")
		}
	})
}

func TestAppRoleAuthLoginSingleflight(t *testing.T) {
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := concurrent.Add(1)
		defer concurrent.Add(-1)
		for {
			m := maxConcurrent.Load()
			if n <= m || maxConcurrent.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{"client_token": "hvs.shared", "lease_duration": 3600},
		})
	}))
	defer srv.Close()

	t.Setenv("BLOCKYARD_VAULT_SECRET_ID", "s")
	a := NewAppRoleAuth(srv.URL, "r", "")

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			if err := a.Login(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if got := maxConcurrent.Load(); got > 1 {
		t.Errorf("max in-flight logins = %d, want 1 (singleflight should coalesce)", got)
	}
}

func TestAppRoleAuthHealthyExpires(t *testing.T) {
	url, _ := approleServer(t, 1) // 1 second lease
	t.Setenv("BLOCKYARD_VAULT_SECRET_ID", "s")

	a := NewAppRoleAuth(url, "r", "")
	if err := a.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !a.Healthy() {
		t.Fatal("Healthy() = false after login with 1s ttl")
	}

	time.Sleep(1200 * time.Millisecond)
	if a.Healthy() {
		t.Error("Healthy() = true after token expiry")
	}
}

func TestAppRoleAuthRunReloginsBeforeExpiry(t *testing.T) {
	// Lease 2s → proactive re-login should fire within ~1s (lease minus
	// reloginBuffer, clamped to 1s minimum). Run for 3s and assert ≥2
	// logins hit the wire.
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{"client_token": "hvs.t", "lease_duration": 2},
		})
	}))
	defer srv.Close()

	t.Setenv("BLOCKYARD_VAULT_SECRET_ID", "s")
	a := NewAppRoleAuth(srv.URL, "r", "")
	if err := a.Login(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()

	time.Sleep(2500 * time.Millisecond)
	cancel()
	<-done

	if got := count.Load(); got < 2 {
		t.Errorf("proactive re-login fired %d times in 2.5s (lease=2s), want ≥2", got)
	}
}

func TestClient403TriggersReloginAndRetries(t *testing.T) {
	// On the first admin call, vault returns 403; the client must
	// re-login and retry. The retry succeeds with the fresh token.
	var (
		relogins atomic.Int32
		attempts atomic.Int32
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sys/mounts":
			attempts.Add(1)
			// Pre-relogin attempts see 403; post-relogin attempts see 200.
			if relogins.Load() == 0 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"secret/": map[string]any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	currentToken := "stale"
	tokenFunc := func() string { return currentToken }
	relogin := func(ctx context.Context) error {
		relogins.Add(1)
		currentToken = "fresh"
		return nil
	}
	c := NewClient(srv.URL, tokenFunc, relogin)

	if err := checkKVMount(context.Background(), c); err != nil {
		t.Fatalf("checkKVMount: %v", err)
	}
	if got := relogins.Load(); got != 1 {
		t.Errorf("relogin called %d times, want 1", got)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("admin call attempted %d times, want 2 (403 + retry)", got)
	}
}

func TestClient403NoReloginWhenNoneConfigured(t *testing.T) {
	// Static admin-token path (no relogin callback): 403 is terminal.
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, func() string { return "t" }, nil)
	if err := checkKVMount(context.Background(), c); err == nil {
		t.Fatal("expected error for persistent 403")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry without relogin)", got)
	}
}
