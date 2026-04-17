//go:build examples

package examples_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Docker Compose lifecycle
// ---------------------------------------------------------------------------

// composeUp starts the given compose file and waits for services to be healthy.
// It registers a cleanup that dumps logs on failure, then tears everything down.
func composeUp(t *testing.T, composeFile string) {
	t.Helper()

	absPath, err := filepath.Abs(composeFile)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	// Base compose flags. When coverage is enabled, layer an override
	// that sets GOCOVERDIR and bind-mounts the host coverage directory
	// into the blockyard container.
	composeFlags := []string{"compose", "-f", absPath}
	if covDir != "" {
		override, err := filepath.Abs("docker-compose.cover.yml")
		if err != nil {
			t.Fatalf("abs path (cover override): %v", err)
		}
		composeFlags = append(composeFlags, "-f", override)
	}

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("docker", append(composeFlags, args...)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if covDir != "" {
			cmd.Env = append(os.Environ(),
				"E2E_GOCOVERDIR="+filepath.Join(covDir, "server"))
		}
		if err := cmd.Run(); err != nil {
			t.Fatalf("docker compose %v: %v", args, err)
		}
	}

	// Register cleanup BEFORE starting — ensures logs are dumped and
	// containers torn down even if compose up fails.
	t.Cleanup(func() {
		if t.Failed() {
			dump := exec.Command("docker", append(composeFlags, "logs", "--no-color")...)
			dump.Stdout = os.Stdout
			dump.Stderr = os.Stderr
			_ = dump.Run()
		}
		down := exec.Command("docker", append(composeFlags, "down", "-v", "--remove-orphans")...)
		down.Stdout = os.Stdout
		down.Stderr = os.Stderr
		_ = down.Run()
	})

	run("up", "-d", "--wait")
}

// ---------------------------------------------------------------------------
// Health polling
// ---------------------------------------------------------------------------

func waitForHealth(t *testing.T, baseURL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("health check at %s/healthz did not pass within %s", baseURL, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// ---------------------------------------------------------------------------
// OIDC login via Dex
// ---------------------------------------------------------------------------

var formActionRe = regexp.MustCompile(`action="([^"]+)"`)

// dexLogin performs the full OIDC login flow against Dex and returns the
// blockyard session cookie. It mirrors the curl-based flow from deploy.sh.
func dexLogin(t *testing.T, baseURL, dexURL, email, password string) []*http.Cookie {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar}

	// Step 1: GET /login — follow redirects to the Dex login form.
	resp, err := client.Get(baseURL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Step 2: Extract form action URL.
	matches := formActionRe.FindSubmatch(body)
	if matches == nil {
		t.Fatalf("no form action found in Dex login page (status %d, body length %d)",
			resp.StatusCode, len(body))
	}
	formAction := strings.ReplaceAll(string(matches[1]), "&amp;", "&")

	// If relative, prepend Dex URL.
	if strings.HasPrefix(formAction, "/") {
		formAction = dexURL + formAction
	}

	// Step 3: POST credentials to Dex, follow redirects back to /callback.
	resp, err = client.PostForm(formAction, url.Values{
		"login":    {email},
		"password": {password},
	})
	if err != nil {
		t.Fatalf("POST dex login form: %v", err)
	}
	resp.Body.Close()

	// Verify we got the session cookie.
	u, _ := url.Parse(baseURL)
	cookies := jar.Cookies(u)
	for _, c := range cookies {
		if c.Name == "blockyard_session" {
			return cookies
		}
	}
	t.Fatalf("dex login did not produce blockyard_session cookie")
	return nil
}

// ---------------------------------------------------------------------------
// PAT creation
// ---------------------------------------------------------------------------

// httpClient is a shared client with a generous timeout for CI.
// Disable keep-alive to prevent stale connection issues on GHA runners.
var httpClient = &http.Client{
	Timeout: 120 * time.Second,
	Transport: &http.Transport{
		DisableKeepAlives: true,
	},
}

func createPAT(t *testing.T, baseURL string, cookies []*http.Cookie) string {
	t.Helper()

	body := `{"name":"e2e-test","expires_in":"1d"}`
	req, _ := http.NewRequest("POST", baseURL+"/api/v1/users/me/tokens",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("create PAT: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create PAT: status %d, body: %s", resp.StatusCode, b)
	}

	var result struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Token == "" {
		t.Fatal("create PAT: empty token in response")
	}
	if !strings.HasPrefix(result.Token, "by_") {
		t.Fatalf("create PAT: token %q does not have by_ prefix", result.Token)
	}
	return result.Token
}

// ---------------------------------------------------------------------------
// CLI helpers
// ---------------------------------------------------------------------------

// runCLI executes the by CLI binary with the given arguments.
// BLOCKYARD_URL and BLOCKYARD_TOKEN are injected via env vars.
// Returns stdout on success; fails the test on non-zero exit.
func runCLI(t *testing.T, serverURL, token string, args ...string) string {
	t.Helper()
	cmd := exec.Command(byBin, args...)
	env := append(os.Environ(),
		"BLOCKYARD_URL="+serverURL,
		"BLOCKYARD_TOKEN="+token,
	)
	if covDir != "" {
		env = append(env, "GOCOVERDIR="+filepath.Join(covDir, "cli"))
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("by %s: %v\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// runCLIJSON executes the by CLI with --json appended and decodes stdout
// into v. Fails the test on non-zero exit or JSON parse errors.
func runCLIJSON(t *testing.T, serverURL, token string, v any, args ...string) {
	t.Helper()
	args = append(args, "--json")
	out := runCLI(t, serverURL, token, args...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("by %s: parse JSON: %v\noutput: %s",
			strings.Join(args, " "), err, out)
	}
}

// runCLIFail executes the by CLI and expects a non-zero exit code.
// Returns stdout and stderr.
func runCLIFail(t *testing.T, serverURL, token string, args ...string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(byBin, args...)
	env := append(os.Environ(),
		"BLOCKYARD_URL="+serverURL,
		"BLOCKYARD_TOKEN="+token,
	)
	if covDir != "" {
		env = append(env, "GOCOVERDIR="+filepath.Join(covDir, "cli"))
	}
	cmd.Env = env
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err == nil {
		t.Fatalf("by %s: expected failure but succeeded\nstdout: %s",
			strings.Join(args, " "), outBuf.String())
	}
	return outBuf.String(), errBuf.String()
}

// waitForAppStatus polls `by get <app> --json` until the app reaches
// the desired status or the timeout expires. Transient CLI errors
// (e.g. connection refused while the server is restarting) are
// tolerated during polling.
func waitForAppStatus(t *testing.T, serverURL, token, app, wantStatus string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command(byBin, "get", app, "--json")
		env := append(os.Environ(),
			"BLOCKYARD_URL="+serverURL,
			"BLOCKYARD_TOKEN="+token,
		)
		if covDir != "" {
			env = append(env, "GOCOVERDIR="+filepath.Join(covDir, "cli"))
		}
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			// Transient error — retry.
			time.Sleep(2 * time.Second)
			continue
		}
		var info map[string]any
		if json.Unmarshal(out, &info) != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if s, _ := info["status"].(string); s == wantStatus {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("app %s did not reach status %q within %s", app, wantStatus, timeout)
}

// copyAppDir copies src to a temp directory so `by deploy` can write
// manifest.json without modifying the source tree.
func copyAppDir(t *testing.T, src string) string {
	t.Helper()
	absSrc, err := filepath.Abs(src)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	dst, err := os.MkdirTemp("", "by-e2e-app-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dst) })

	err = filepath.Walk(absSrc, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(absSrc, path)
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(target, data, info.Mode())
	})
	if err != nil {
		t.Fatalf("copy app dir: %v", err)
	}
	return dst
}

// ---------------------------------------------------------------------------
// Credential enrollment
// ---------------------------------------------------------------------------

func enrollCredentialWithPAT(t *testing.T, baseURL, token, service, apiKey string) {
	t.Helper()
	body := fmt.Sprintf(`{"api_key":%q}`, apiKey)
	req, _ := http.NewRequest("POST",
		baseURL+"/api/v1/users/me/credentials/"+service,
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("enroll credential: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("enroll credential: expected 204, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Vault direct read
// ---------------------------------------------------------------------------

func readVaultSecret(t *testing.T, vaultURL, token, path string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", vaultURL+"/v1/"+path, nil)
	req.Header.Set("X-Vault-Token", token)

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("read vault secret: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("read vault: status %d, body: %s", resp.StatusCode, b)
	}

	var envelope struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&envelope)
	return envelope.Data.Data
}

// ---------------------------------------------------------------------------
// App page fetch (proxy — requires session cookies, not CLI)
// ---------------------------------------------------------------------------

// newProxyClient returns an http.Client seeded with the given auth cookies
// and a cookie jar that retains Set-Cookie values across requests. Reusing
// the same client across fetchAppPage and dialAppWebSocket keeps the proxy
// session cookie stable so subsequent requests route to the same backend
// worker instead of cold-starting a fresh one per call.
func newProxyClient(t *testing.T, baseURL string, cookies []*http.Cookie) *http.Client {
	t.Helper()
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse baseURL: %v", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	jar.SetCookies(u, cookies)
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func fetchAppPage(t *testing.T, client *http.Client, baseURL, appName string, timeout time.Duration) (int, string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		resp, err := client.Get(baseURL + "/app/" + appName + "/")
		if err != nil {
			if time.Now().After(deadline) {
				t.Fatalf("fetch app page: %v", err)
			}
			time.Sleep(2 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// 502/503 = worker not ready yet, keep polling.
		if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable {
			if time.Now().After(deadline) {
				t.Fatalf("app page still %d after %s", resp.StatusCode, timeout)
			}
			time.Sleep(2 * time.Second)
			continue
		}
		return resp.StatusCode, string(body)
	}
}

// fetchAppPageNoRedirect fetches the app page without cookies and without following redirects.
func fetchAppPageNoRedirect(t *testing.T, baseURL, appName string) (int, string) {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(baseURL + "/app/" + appName + "/")
	if err != nil {
		t.Fatalf("fetch app page (no auth): %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

// ---------------------------------------------------------------------------
// WebSocket helper (proxy — requires session cookies, not CLI)
// ---------------------------------------------------------------------------

func dialAppWebSocket(t *testing.T, client *http.Client, baseURL, appName string) {
	t.Helper()

	// Use http:// (not ws://) since we do a raw HTTP upgrade via RoundTrip.
	wsURL := baseURL + "/app/" + appName + "/websocket/"

	// Build cookie header from the client jar so the blockyard_session
	// cookie (set on the first fetchAppPage) is included. All retry
	// attempts therefore share the same backend worker.
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse baseURL: %v", err)
	}
	var cookieStrs []string
	for _, c := range client.Jar.Cookies(u) {
		cookieStrs = append(cookieStrs, c.Name+"="+c.Value)
	}

	// Retry on transient errors (EOF, connection reset) common on CI.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			t.Logf("websocket dial: retrying (attempt %d) after: %v", attempt+1, lastErr)
			time.Sleep(2 * time.Second)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		header := http.Header{}
		if len(cookieStrs) > 0 {
			header.Set("Cookie", strings.Join(cookieStrs, "; "))
		}

		req, _ := http.NewRequestWithContext(ctx, "GET", wsURL, nil)
		req.Header = header
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		req.Header.Set("Sec-WebSocket-Version", "13")

		resp, err := http.DefaultTransport.RoundTrip(req)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusSwitchingProtocols {
			return
		}
		lastErr = fmt.Errorf("status %d", resp.StatusCode)
	}
	t.Fatalf("websocket dial: all attempts failed, last error: %v", lastErr)
}

// ---------------------------------------------------------------------------
// Raw HTTP helper for edge cases without CLI equivalent
// ---------------------------------------------------------------------------

// apiPost performs a POST request with Bearer auth. Returns the response
// for the caller to inspect (caller must close body).
func apiPost(t *testing.T, baseURL, token, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", baseURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
