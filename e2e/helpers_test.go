//go:build e2e

package e2e_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("docker", append([]string{"compose", "-f", absPath}, args...)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("docker compose %v: %v", args, err)
		}
	}

	run("up", "-d", "--wait")

	t.Cleanup(func() {
		if t.Failed() {
			dump := exec.Command("docker", "compose", "-f", absPath, "logs", "--no-color")
			dump.Stdout = os.Stdout
			dump.Stderr = os.Stderr
			_ = dump.Run()
		}
		down := exec.Command("docker", "compose", "-f", absPath, "down", "-v", "--remove-orphans")
		down.Stdout = os.Stdout
		down.Stderr = os.Stderr
		_ = down.Run()
	})
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

func createPAT(t *testing.T, baseURL string, cookies []*http.Cookie) string {
	t.Helper()

	body := `{"name":"e2e-test","expires_in":"1d"}`
	req, _ := http.NewRequest("POST", baseURL+"/api/v1/users/me/tokens",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create PAT: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
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
// API Client
// ---------------------------------------------------------------------------

type APIClient struct {
	BaseURL string
	Token   string
}

func (c *APIClient) do(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func (c *APIClient) doOctet(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	return http.DefaultClient.Do(req)
}

// CreateApp creates an app and returns its ID. Handles 409 (already exists)
// by listing apps and finding the ID.
func (c *APIClient) CreateApp(t *testing.T, name string) string {
	t.Helper()

	resp, err := c.do("POST", "/api/v1/apps", strings.NewReader(fmt.Sprintf(`{"name":%q}`, name)))
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusCreated {
		var result struct {
			ID string `json:"id"`
		}
		json.NewDecoder(resp.Body).Decode(&result)
		return result.ID
	}

	if resp.StatusCode == http.StatusConflict {
		// App already exists — find it.
		return c.findAppByName(t, name)
	}

	b, _ := io.ReadAll(resp.Body)
	t.Fatalf("create app %q: status %d, body: %s", name, resp.StatusCode, b)
	return ""
}

func (c *APIClient) findAppByName(t *testing.T, name string) string {
	t.Helper()
	resp, err := c.do("GET", "/api/v1/apps", nil)
	if err != nil {
		t.Fatalf("list apps: %v", err)
	}
	defer resp.Body.Close()

	var apps []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	json.NewDecoder(resp.Body).Decode(&apps)
	for _, a := range apps {
		if a.Name == name {
			return a.ID
		}
	}
	t.Fatalf("app %q not found in list", name)
	return ""
}

func (c *APIClient) UpdateApp(t *testing.T, id string, body string) {
	t.Helper()
	resp, err := c.do("PATCH", "/api/v1/apps/"+id, strings.NewReader(body))
	if err != nil {
		t.Fatalf("update app: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update app: status %d", resp.StatusCode)
	}
}

func (c *APIClient) UploadBundle(t *testing.T, appID string, tarGz []byte) (taskID, bundleID string) {
	t.Helper()
	resp, err := c.doOctet("POST", "/api/v1/apps/"+appID+"/bundles", bytes.NewReader(tarGz))
	if err != nil {
		t.Fatalf("upload bundle: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload bundle: status %d, body: %s", resp.StatusCode, b)
	}

	var result struct {
		TaskID   string `json:"task_id"`
		BundleID string `json:"bundle_id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.TaskID, result.BundleID
}

func (c *APIClient) PollTask(t *testing.T, taskID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp, err := c.do("GET", "/api/v1/tasks/"+taskID, nil)
		if err != nil {
			t.Fatalf("poll task: %v", err)
		}
		var result struct {
			Status string `json:"status"`
		}
		json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()

		switch result.Status {
		case "completed":
			return
		case "failed":
			t.Fatalf("task %s failed", taskID)
		}

		if time.Now().After(deadline) {
			t.Fatalf("task %s did not complete within %s (last status: %s)",
				taskID, timeout, result.Status)
		}
		time.Sleep(5 * time.Second)
	}
}

func (c *APIClient) StartApp(t *testing.T, appID string) string {
	t.Helper()
	resp, err := c.do("POST", "/api/v1/apps/"+appID+"/start", nil)
	if err != nil {
		t.Fatalf("start app: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("start app: status %d, body: %s", resp.StatusCode, b)
	}
	var result struct {
		WorkerID string `json:"worker_id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.WorkerID
}

func (c *APIClient) StopApp(t *testing.T, appID string) {
	t.Helper()
	resp, err := c.do("POST", "/api/v1/apps/"+appID+"/stop", nil)
	if err != nil {
		t.Fatalf("stop app: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stop app: status %d, body: %s", resp.StatusCode, b)
	}

	// If async stop, poll the task.
	var result struct {
		TaskID string `json:"task_id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.TaskID != "" {
		c.PollTask(t, result.TaskID, 60*time.Second)
	}
}

func (c *APIClient) DeleteApp(t *testing.T, appID string) {
	t.Helper()
	resp, err := c.do("DELETE", "/api/v1/apps/"+appID, nil)
	if err != nil {
		t.Fatalf("delete app: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete app: status %d", resp.StatusCode)
	}
}

func (c *APIClient) GetApp(t *testing.T, id string) (int, map[string]any) {
	t.Helper()
	resp, err := c.do("GET", "/api/v1/apps/"+id, nil)
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	defer resp.Body.Close()
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	return resp.StatusCode, result
}

// DeleteAppRaw does DELETE without failing the test; returns status code.
func (c *APIClient) DeleteAppRaw(appID string) int {
	resp, err := c.do("DELETE", "/api/v1/apps/"+appID, nil)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// ---------------------------------------------------------------------------
// Credential enrollment
// ---------------------------------------------------------------------------

func enrollCredential(t *testing.T, baseURL string, cookies []*http.Cookie, service, apiKey string) {
	t.Helper()
	body := fmt.Sprintf(`{"api_key":%q}`, apiKey)
	req, _ := http.NewRequest("POST",
		baseURL+"/api/v1/users/me/credentials/"+service,
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("enroll credential: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("enroll credential: expected 204, got %d", resp.StatusCode)
	}
}

// enrollCredentialWithPAT enrolls a credential using a bearer token.
func enrollCredentialWithPAT(t *testing.T, baseURL, token, service, apiKey string) {
	t.Helper()
	body := fmt.Sprintf(`{"api_key":%q}`, apiKey)
	req, _ := http.NewRequest("POST",
		baseURL+"/api/v1/users/me/credentials/"+service,
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
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

	resp, err := http.DefaultClient.Do(req)
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
// Bundle creation from directory
// ---------------------------------------------------------------------------

func makeBundle(t *testing.T, dir string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs dir: %v", err)
	}

	err = filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(absDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel

		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		t.Fatalf("create tar.gz: %v", err)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// App page fetch
// ---------------------------------------------------------------------------

func fetchAppPage(t *testing.T, baseURL, appName string, cookies []*http.Cookie, timeout time.Duration) (int, string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		req, _ := http.NewRequest("GET", baseURL+"/app/"+appName+"/", nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		// Don't follow redirects automatically — we want to see the redirect.
		client := &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := client.Do(req)
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
// WebSocket helper
// ---------------------------------------------------------------------------

func dialAppWebSocket(t *testing.T, baseURL, appName string, cookies []*http.Cookie) {
	t.Helper()

	// Convert http:// to ws://
	wsURL := strings.Replace(baseURL, "http://", "ws://", 1) +
		"/app/" + appName + "/websocket/"

	// Build cookie header.
	var cookieStrs []string
	for _, c := range cookies {
		cookieStrs = append(cookieStrs, c.Name+"="+c.Value)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	header := http.Header{}
	if len(cookieStrs) > 0 {
		header.Set("Cookie", strings.Join(cookieStrs, "; "))
	}

	// Use coder/websocket via the standard library's net/http upgrade path.
	// We do a manual websocket handshake to keep the dependency minimal.
	req, _ := http.NewRequestWithContext(ctx, "GET", wsURL, nil)
	req.Header = header
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("websocket: expected 101 Switching Protocols, got %d: %s",
			resp.StatusCode, body)
	}
}
