package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/server"
)

const (
	testBootstrapToken = "by_bootstrap_for_tests"
	testInitialAdmin   = "admin-sub"
)

// armBootstrap configures srv for bootstrap exchange and returns the
// httptest URL to POST against. Caller is responsible for calling
// testServer first.
func armBootstrap(t *testing.T, srv *server.Server) {
	t.Helper()
	if srv.Config.OIDC == nil {
		srv.Config.OIDC = &config.OidcConfig{}
	}
	srv.Config.OIDC.InitialAdmin = testInitialAdmin
	srv.BootstrapTokenHash = auth.HashPAT(testBootstrapToken)
	srv.BootstrapRedeemed.Store(false)
}

func bootstrapReq(token string, body io.Reader) *http.Request {
	req, _ := http.NewRequest("POST", "/api/v1/bootstrap", body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestBootstrap_NotConfigured(t *testing.T) {
	srv, ts := testServer(t)
	// Deliberately do NOT arm bootstrap — leave hash empty.
	_ = srv

	resp, err := http.Post(ts.URL+"/api/v1/bootstrap", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestBootstrap_AlreadyRedeemed(t *testing.T) {
	srv, ts := testServer(t)
	armBootstrap(t, srv)
	srv.BootstrapRedeemed.Store(true)

	req := bootstrapReq(testBootstrapToken, strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(mustURL(t, req, ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("status = %d, want 410", resp.StatusCode)
	}
}

func TestBootstrap_NoBearer(t *testing.T) {
	srv, ts := testServer(t)
	armBootstrap(t, srv)

	req := bootstrapReq("", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(mustURL(t, req, ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestBootstrap_WrongToken(t *testing.T) {
	srv, ts := testServer(t)
	armBootstrap(t, srv)

	req := bootstrapReq("by_completely_wrong_value", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(mustURL(t, req, ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if srv.BootstrapRedeemed.Load() {
		t.Error("BootstrapRedeemed should remain false on wrong token")
	}
}

func TestBootstrap_BadBody(t *testing.T) {
	srv, ts := testServer(t)
	armBootstrap(t, srv)

	req := bootstrapReq(testBootstrapToken, strings.NewReader(`not json`))
	resp, err := http.DefaultClient.Do(mustURL(t, req, ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if srv.BootstrapRedeemed.Load() {
		t.Error("BootstrapRedeemed should remain false on bad body")
	}
}

func TestBootstrap_BadExpires(t *testing.T) {
	srv, ts := testServer(t)
	armBootstrap(t, srv)

	body := strings.NewReader(`{"expires_in":"forever"}`)
	req := bootstrapReq(testBootstrapToken, body)
	resp, err := http.DefaultClient.Do(mustURL(t, req, ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestBootstrap_HappyPath(t *testing.T) {
	srv, ts := testServer(t)
	armBootstrap(t, srv)

	body := strings.NewReader(`{"name":"my-pat","expires_in":"24h"}`)
	req := bootstrapReq(testBootstrapToken, body)
	resp, err := http.DefaultClient.Do(mustURL(t, req, ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if s, _ := got["name"].(string); s != "my-pat" {
		t.Errorf("name = %q, want my-pat", s)
	}
	if s, _ := got["token"].(string); s == "" {
		t.Error("token missing from response")
	}
	if s, _ := got["expires_at"].(string); s == "" {
		t.Error("expires_at should be set when expires_in given")
	}

	if !srv.BootstrapRedeemed.Load() {
		t.Error("BootstrapRedeemed should be true after success")
	}

	// Initial admin user must exist with admin role.
	user, err := srv.DB.GetUser(testInitialAdmin)
	if err != nil {
		t.Fatalf("GetUser(%q): %v", testInitialAdmin, err)
	}
	if user.Role != "admin" {
		t.Errorf("initial admin role = %q, want admin", user.Role)
	}
}

func TestBootstrap_DefaultName(t *testing.T) {
	srv, ts := testServer(t)
	armBootstrap(t, srv)

	req := bootstrapReq(testBootstrapToken, strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(mustURL(t, req, ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if s, _ := got["name"].(string); s != "bootstrap" {
		t.Errorf("name = %q, want bootstrap (default)", s)
	}
}

func TestBootstrap_BurnsToken(t *testing.T) {
	srv, ts := testServer(t)
	armBootstrap(t, srv)

	// First call succeeds.
	req := bootstrapReq(testBootstrapToken, strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(mustURL(t, req, ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first call status = %d, want 201", resp.StatusCode)
	}

	// Second call with the same token returns 410.
	req2 := bootstrapReq(testBootstrapToken, strings.NewReader(`{}`))
	resp2, err := http.DefaultClient.Do(mustURL(t, req2, ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusGone {
		t.Errorf("second call status = %d, want 410", resp2.StatusCode)
	}
}

// TestBootstrap_ConcurrentRedeem fires N requests in parallel and asserts
// exactly one wins. Guards the CompareAndSwap that burns the token.
func TestBootstrap_ConcurrentRedeem(t *testing.T) {
	srv, ts := testServer(t)
	armBootstrap(t, srv)

	const N = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		gone    int
	)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := bootstrapReq(testBootstrapToken, bytes.NewReader([]byte(`{}`)))
			resp, err := http.DefaultClient.Do(mustURL(t, req, ts.URL))
			if err != nil {
				return
			}
			defer resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			switch resp.StatusCode {
			case http.StatusCreated:
				created++
			case http.StatusGone:
				gone++
			}
		}()
	}
	wg.Wait()

	if created != 1 {
		t.Errorf("created = %d, want exactly 1", created)
	}
	if created+gone != N {
		t.Errorf("created + gone = %d, want %d (all responses accounted for)", created+gone, N)
	}
}

// mustURL rewrites the request URL to point at the httptest server.
// Allows building requests with relative paths above for readability.
func mustURL(t *testing.T, req *http.Request, base string) *http.Request {
	t.Helper()
	u, err := req.URL.Parse(base + req.URL.Path)
	if err != nil {
		t.Fatal(err)
	}
	req.URL = u
	req.Host = u.Host
	return req
}
