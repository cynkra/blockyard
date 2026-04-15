package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestAppRoleCredsResolve(t *testing.T) {
	dir := t.TempDir()

	t.Run("env only", func(t *testing.T) {
		creds := AppRoleCreds{RoleIDEnv: "env-role", SecretIDEnv: "env-secret"}
		roleID, secretID, err := creds.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if roleID != "env-role" || secretID != "env-secret" {
			t.Errorf("got role=%q secret=%q", roleID, secretID)
		}
	})

	t.Run("file only", func(t *testing.T) {
		rolePath := filepath.Join(dir, "role")
		secretPath := filepath.Join(dir, "secret")
		if err := os.WriteFile(rolePath, []byte("file-role\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(secretPath, []byte("  file-secret  "), 0o600); err != nil {
			t.Fatal(err)
		}
		creds := AppRoleCreds{RoleIDFile: rolePath, SecretIDFile: secretPath}
		roleID, secretID, err := creds.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if roleID != "file-role" || secretID != "file-secret" {
			t.Errorf("got role=%q secret=%q", roleID, secretID)
		}
	})

	t.Run("file wins over env", func(t *testing.T) {
		path := filepath.Join(dir, "precedence")
		if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
			t.Fatal(err)
		}
		creds := AppRoleCreds{
			RoleIDEnv:   "from-env",
			RoleIDFile:  path,
			SecretIDEnv: "se",
		}
		roleID, _, err := creds.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if roleID != "from-file" {
			t.Errorf("role_id = %q, want from-file", roleID)
		}
	})

	t.Run("file re-read on each call", func(t *testing.T) {
		path := filepath.Join(dir, "rotating")
		if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
			t.Fatal(err)
		}
		creds := AppRoleCreds{RoleIDFile: path, SecretIDEnv: "se"}
		v1, _, err := creds.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("v2"), 0o600); err != nil {
			t.Fatal(err)
		}
		v2, _, err := creds.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if v1 != "v1" || v2 != "v2" {
			t.Errorf("got v1=%q v2=%q", v1, v2)
		}
	})

	t.Run("missing everything", func(t *testing.T) {
		creds := AppRoleCreds{}
		_, _, err := creds.Resolve()
		if err == nil {
			t.Fatal("expected error when no source is set")
		}
		if !strings.Contains(err.Error(), "BLOCKYARD_OPENBAO_ROLE_ID") {
			t.Errorf("error should name the missing env var: %v", err)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(dir, "empty")
		if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		creds := AppRoleCreds{RoleIDFile: path, SecretIDEnv: "se"}
		_, _, err := creds.Resolve()
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Errorf("expected empty-file error, got %v", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		creds := AppRoleCreds{RoleIDFile: filepath.Join(dir, "nope"), SecretIDEnv: "se"}
		_, _, err := creds.Resolve()
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})
}

func TestAppRoleAuthLogin(t *testing.T) {
	var loginCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loginCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "tok-initial",
				"lease_duration": 300,
			},
		})
	}))
	defer srv.Close()

	auth := NewAppRoleAuth(srv.URL, AppRoleCreds{RoleIDEnv: "r", SecretIDEnv: "s"})

	if auth.Token() != "" {
		t.Error("expected empty token before Login")
	}

	if err := auth.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth.Token() != "tok-initial" {
		t.Errorf("token = %q", auth.Token())
	}
	if loginCount.Load() != 1 {
		t.Errorf("login count = %d, want 1", loginCount.Load())
	}
}

func TestAppRoleAuthReauthRotatesToken(t *testing.T) {
	var loginCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := loginCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "tok-" + string(rune('0'+n)),
				"lease_duration": 300,
			},
		})
	}))
	defer srv.Close()

	auth := NewAppRoleAuth(srv.URL, AppRoleCreds{RoleIDEnv: "r", SecretIDEnv: "s"})
	if err := auth.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := auth.Token()
	if err := auth.Reauth(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if auth.Token() == first {
		t.Error("expected token to rotate after Reauth")
	}
	if loginCount.Load() != 2 {
		t.Errorf("login count = %d, want 2", loginCount.Load())
	}
}

func TestAppRoleAuthReauthSkipsIfTokenAlreadyRotated(t *testing.T) {
	var loginCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := loginCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "tok-" + string(rune('0'+n)),
				"lease_duration": 300,
			},
		})
	}))
	defer srv.Close()

	auth := NewAppRoleAuth(srv.URL, AppRoleCreds{RoleIDEnv: "r", SecretIDEnv: "s"})
	if err := auth.Login(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Caller "observed" a stale token different from the current one.
	// Reauth should be a no-op — the caller just needs to retry with
	// the current Token.
	before := auth.Token()
	if err := auth.Reauth(context.Background(), "stale-token"); err != nil {
		t.Fatal(err)
	}
	if auth.Token() != before {
		t.Error("Reauth should not rotate when observed token is stale")
	}
	if loginCount.Load() != 1 {
		t.Errorf("login count = %d, want 1 (no second login)", loginCount.Load())
	}
}

func TestAppRoleAuthReauthCoalescesConcurrentCalls(t *testing.T) {
	var loginCount atomic.Int32
	var inLogin atomic.Int32
	enter := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := loginCount.Add(1)
		if n >= 2 {
			// Second login observed = singleflight broken.
			inLogin.Add(1)
			select {
			case enter <- struct{}{}:
			default:
			}
			<-release
		} else {
			// First login blocks long enough for concurrent callers
			// to pile up and coalesce.
			inLogin.Add(1)
			select {
			case enter <- struct{}{}:
			default:
			}
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "tok-v" + string(rune('0'+n)),
				"lease_duration": 300,
			},
		})
	}))
	defer srv.Close()

	auth := NewAppRoleAuth(srv.URL, AppRoleCreds{RoleIDEnv: "r", SecretIDEnv: "s"})

	// Trigger the leader Reauth; it blocks inside the fake server.
	var leaderErr error
	leaderDone := make(chan struct{})
	go func() {
		leaderErr = auth.Reauth(context.Background(), "")
		close(leaderDone)
	}()
	<-enter // leader is now in the fake server

	// Kick off N followers that should all coalesce onto the leader.
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = auth.Reauth(context.Background(), "")
		}(i)
	}

	// Give the followers a moment to enter the refresh path and
	// start waiting on the leader's inflight channel.
	time.Sleep(50 * time.Millisecond)

	close(release) // let the leader finish
	wg.Wait()
	<-leaderDone

	if leaderErr != nil {
		t.Fatalf("leader reauth: %v", leaderErr)
	}
	for i, e := range errs {
		if e != nil {
			t.Errorf("follower %d: %v", i, e)
		}
	}
	if got := loginCount.Load(); got != 1 {
		t.Errorf("login count = %d, want 1 (singleflight)", got)
	}
}

func TestStaticAdminReauthFails(t *testing.T) {
	admin := StaticAdmin(func() string { return "fixed" })
	if admin.Token() != "fixed" {
		t.Errorf("token = %q", admin.Token())
	}
	if err := admin.Reauth(context.Background(), "fixed"); err == nil {
		t.Fatal("expected error from static admin Reauth")
	}
}
