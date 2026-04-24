package integration

import (
	"context"
	"encoding/json"
	"fmt"
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
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
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

	// The handler stays in flight until close(release), so the wait
	// only needs to cover goroutine scheduling — not an HTTP round
	// trip — for the late callers to arrive at singleflight.Do.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("HTTP handler called %d times, want 1 (singleflight should coalesce)", got)
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

func TestAppRoleAuthRunRetriesWithExponentialBackoff(t *testing.T) {
	// With the fixed 10s backoff, Run would fire at most once in 2.5s
	// after a failure. Exponential backoff starts at 1s, so we should
	// see multiple retries in that window.
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Setenv("BLOCKYARD_VAULT_SECRET_ID", "s")
	a := NewAppRoleAuth(srv.URL, "r", "")
	// Seed an initial failure so Run starts in the retry path.
	_ = a.Login(context.Background())
	a.timerMu.Lock()
	a.nextAt = time.Now()
	a.timerMu.Unlock()
	initial := count.Load()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()

	time.Sleep(2500 * time.Millisecond)
	cancel()
	<-done

	if got := count.Load() - initial; got < 2 {
		t.Errorf("expected ≥2 retries in 2.5s with exp backoff, got %d", got)
	}
}

func TestAppRoleAuthRunReArmsOnExternalLogin(t *testing.T) {
	// A Login that completes outside Run's timer path (startup, a
	// 403-driven retry) must push the proactive timer forward. If the
	// already-armed timer fires anyway it drives a redundant second
	// login at its original deadline. Verify that an external Login at
	// T≈500ms prevents a proactive fire at the original T≈1s.
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
	defer cancel()
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()

	// External re-login before the proactive timer would fire.
	time.Sleep(500 * time.Millisecond)
	if err := a.Login(ctx); err != nil {
		t.Fatal(err)
	}
	after := count.Load()
	if after != 2 {
		t.Fatalf("count after external Login = %d, want 2 (initial + external)", after)
	}

	// The original timer was armed for ~T=1s. With the re-arm it now
	// fires at ~T=1.5s. Sleep past the original deadline but before
	// the re-armed one.
	time.Sleep(700 * time.Millisecond)
	cancel()
	<-done

	if got := count.Load(); got != after {
		t.Errorf("proactive timer fired at its original deadline: count=%d, want %d", got, after)
	}
}

func TestAppRoleAuthRunHandlesLoginFailure(t *testing.T) {
	// When the proactive Login fails (e.g. vault rejects us), Run
	// marks the component unhealthy and pushes nextAt out by 10s so
	// the loop retries on a coarse schedule rather than tight-looping.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	t.Setenv("BLOCKYARD_VAULT_SECRET_ID", "s")
	a := NewAppRoleAuth(srv.URL, "r", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()

	// Initial nextAt is zero so waitUntilNext clamps to 1s. Give Run
	// time to fire the timer, attempt Login (403), take the error path.
	time.Sleep(1400 * time.Millisecond)
	if a.Healthy() {
		t.Error("Healthy() = true after failed proactive Login")
	}
	cancel()
	<-done
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

// wrappedApproleServer returns a test vault that answers both
// POST /v1/sys/wrapping/unwrap and POST /v1/auth/approle/login.
// Each unwrap call consumes one token from wrapTokens in order,
// mapping it to the matching plaintext from plaintexts. The approle
// handler records every secret_id it received so tests can assert
// which plaintext reached the login endpoint.
func wrappedApproleServer(t *testing.T, wrapTokens, plaintexts []string) (url string, seenSecrets *[]string, unwrapCount, loginCount *atomic.Int32) {
	t.Helper()
	if len(wrapTokens) != len(plaintexts) {
		t.Fatalf("wrapTokens and plaintexts length mismatch: %d vs %d", len(wrapTokens), len(plaintexts))
	}
	var (
		mu          sync.Mutex
		seen        []string
		unwraps     atomic.Int32
		logins      atomic.Int32
		tokenToPlain = make(map[string]string, len(wrapTokens))
		consumed    = make(map[string]bool, len(wrapTokens))
	)
	for i, tok := range wrapTokens {
		tokenToPlain[tok] = plaintexts[i]
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sys/wrapping/unwrap":
			unwraps.Add(1)
			tok := r.Header.Get("X-Vault-Token")
			mu.Lock()
			plain, ok := tokenToPlain[tok]
			already := consumed[tok]
			if ok && !already {
				consumed[tok] = true
			}
			mu.Unlock()
			if !ok || already {
				// Unknown wrap token or already-consumed — matches
				// vault's real tamper behaviour.
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"secret_id": plain},
			})
		case "/v1/auth/approle/login":
			logins.Add(1)
			var body struct {
				RoleID   string `json:"role_id"`
				SecretID string `json:"secret_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			seen = append(seen, body.SecretID)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"auth": map[string]any{
					"client_token":   fmt.Sprintf("hvs.login-%d", logins.Load()),
					"lease_duration": 3600,
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &seen, &unwraps, &logins
}

func TestAppRoleAuthUnwrapsWrappedSecretID(t *testing.T) {
	url, seen, unwraps, logins := wrappedApproleServer(t,
		[]string{"wrap-token-A"}, []string{"real-secret-A"})
	dir := t.TempDir()
	path := filepath.Join(dir, "secret_id")
	if err := os.WriteFile(path, []byte("wrap-token-A\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAppRoleAuth(url, "my-role", path).WithSecretIDWrapped(true)
	if err := a.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !a.Healthy() {
		t.Error("Healthy() = false after successful wrapped login")
	}
	if got := unwraps.Load(); got != 1 {
		t.Errorf("unwrap calls = %d, want 1", got)
	}
	if got := logins.Load(); got != 1 {
		t.Errorf("login calls = %d, want 1", got)
	}
	if len(*seen) != 1 || (*seen)[0] != "real-secret-A" {
		t.Errorf("server received secret_ids %v, want [real-secret-A] (unwrapped plaintext)", *seen)
	}
}

func TestAppRoleAuthWrappedCachesPlaintextAcrossLogins(t *testing.T) {
	// Server will reject a second unwrap of the same wrap token. The
	// cache must skip the second unwrap so a proactive re-login works
	// on an unchanged file.
	url, seen, unwraps, _ := wrappedApproleServer(t,
		[]string{"wrap-A"}, []string{"plain-A"})
	dir := t.TempDir()
	path := filepath.Join(dir, "secret_id")
	if err := os.WriteFile(path, []byte("wrap-A"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAppRoleAuth(url, "r", path).WithSecretIDWrapped(true)
	if err := a.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("second login failed (cache miss would re-unwrap a consumed token): %v", err)
	}
	if got := unwraps.Load(); got != 1 {
		t.Errorf("unwrap calls = %d, want 1 (cache should coalesce re-logins on unchanged file)", got)
	}
	if len(*seen) != 2 || (*seen)[0] != "plain-A" || (*seen)[1] != "plain-A" {
		t.Errorf("server received %v, want [plain-A plain-A]", *seen)
	}
}

func TestAppRoleAuthWrappedReunwrapsAfterRotation(t *testing.T) {
	url, seen, unwraps, _ := wrappedApproleServer(t,
		[]string{"wrap-A", "wrap-B"}, []string{"plain-A", "plain-B"})
	dir := t.TempDir()
	path := filepath.Join(dir, "secret_id")
	if err := os.WriteFile(path, []byte("wrap-A"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAppRoleAuth(url, "r", path).WithSecretIDWrapped(true)
	if err := a.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Rotate the on-disk wrap token.
	if err := os.WriteFile(path, []byte("wrap-B"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Login(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := unwraps.Load(); got != 2 {
		t.Errorf("unwrap calls = %d, want 2 (one per distinct wrap token)", got)
	}
	if len(*seen) != 2 || (*seen)[0] != "plain-A" || (*seen)[1] != "plain-B" {
		t.Errorf("server received %v, want [plain-A plain-B]", *seen)
	}
}

func TestAppRoleAuthWrappedUnwrapDecodeErrorIsFatal(t *testing.T) {
	// sys/wrapping/unwrap returns 200 with a non-JSON body: the
	// unwrap must error out rather than treat the garbage as a
	// successful response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sys/wrapping/unwrap" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "secret_id")
	if err := os.WriteFile(path, []byte("wrap"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAppRoleAuth(srv.URL, "r", path).WithSecretIDWrapped(true)
	if err := a.Login(context.Background()); err == nil {
		t.Fatal("expected Login to fail when unwrap body is malformed JSON")
	}
	if a.Healthy() {
		t.Error("Healthy() = true after decode failure")
	}
}

func TestAppRoleAuthWrappedUnwrapEmptyResponseIsFatal(t *testing.T) {
	// sys/wrapping/unwrap returns 200 with no secret_id. Without this
	// guard we'd attempt an AppRole login with an empty secret_id,
	// which is a distinct, more confusing failure mode.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sys/wrapping/unwrap" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "secret_id")
	if err := os.WriteFile(path, []byte("wrap"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAppRoleAuth(srv.URL, "r", path).WithSecretIDWrapped(true)
	err := a.Login(context.Background())
	if err == nil {
		t.Fatal("expected Login to fail when unwrap returns empty secret_id")
	}
	if !strings.Contains(err.Error(), "empty secret_id") {
		t.Errorf("error %v does not mention empty secret_id", err)
	}
}

func TestAppRoleAuthWrappedUnwrapNetworkErrorIsFatal(t *testing.T) {
	// Point at a server that has already been closed: httpClient.Do
	// fails at the transport layer, exercising the dial-error return
	// path distinct from the non-200-status path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "secret_id")
	if err := os.WriteFile(path, []byte("wrap"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAppRoleAuth(addr, "r", path).WithSecretIDWrapped(true)
	if err := a.Login(context.Background()); err == nil {
		t.Fatal("expected Login to fail when unwrap cannot reach the server")
	}
}

func TestAppRoleAuthWrappedUnwrapFailureIsFatal(t *testing.T) {
	// sys/wrapping/unwrap returns 400 (e.g. tampered/consumed token);
	// the login must fail and Healthy() must be false.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sys/wrapping/unwrap" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "secret_id")
	if err := os.WriteFile(path, []byte("bad-wrap-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAppRoleAuth(srv.URL, "r", path).WithSecretIDWrapped(true)
	if err := a.Login(context.Background()); err == nil {
		t.Fatal("expected Login to fail when unwrap returns 400")
	}
	if a.Healthy() {
		t.Error("Healthy() = true after unwrap failure")
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
