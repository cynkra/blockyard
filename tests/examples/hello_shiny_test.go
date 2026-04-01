//go:build examples

package examples_test

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestHelloShinyBrowserLogin verifies the full OIDC login flow using a single
// cookie-jar-enabled HTTP client, simulating a real browser. This catches
// issues where the session cookie is set in the callback response but not
// properly sent (or recognised) on the subsequent redirect — a scenario
// that manual cookie injection in the other e2e tests cannot detect.
func TestHelloShinyBrowserLogin(t *testing.T) {
	composeUp(t, "../../examples/hello-shiny/docker-compose.yml")

	baseURL := "http://localhost:8080"
	dexURL := "http://localhost:5556"

	waitForHealth(t, baseURL, 60*time.Second)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar}

	// Step 1: GET /login — follows redirects to the Dex login form.
	resp, err := client.Get(baseURL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	matches := formActionRe.FindSubmatch(body)
	if matches == nil {
		t.Fatalf("no form action on Dex login page (status %d)", resp.StatusCode)
	}
	formAction := strings.ReplaceAll(string(matches[1]), "&amp;", "&")
	if strings.HasPrefix(formAction, "/") {
		formAction = dexURL + formAction
	}

	// Step 2: POST credentials → Dex authenticates → redirects to
	// /callback → callback sets session cookie → redirects to /.
	// The jar-enabled client follows the entire chain automatically.
	resp, err = client.PostForm(formAction, url.Values{
		"login":    {dexEmail1},
		"password": {dexPassword},
	})
	if err != nil {
		t.Fatalf("POST dex login: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	// After the full redirect chain we must land on the authenticated
	// apps page, NOT the unauthenticated landing page.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status after login: got %d, want 200", resp.StatusCode)
	}
	page := string(body)
	if strings.Contains(page, `class="sign-in"`) {
		t.Fatal("login redirect chain landed on landing page (sign-in button present) — session cookie not recognised")
	}
	if !strings.Contains(page, `class="left-nav"`) {
		t.Fatal("login redirect chain did not produce authenticated page (left-nav missing)")
	}

	// Step 3: Visit / again using the same client. The jar must
	// automatically send the session cookie without manual injection.
	resp, err = client.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET / (post-login): %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	page = string(body)
	if strings.Contains(page, `class="sign-in"`) {
		t.Fatal("second visit to / shows landing page — session cookie not persisted in jar")
	}
	if !strings.Contains(page, `class="left-nav"`) {
		t.Fatal("second visit to / not authenticated (left-nav missing)")
	}
}

func TestHelloShiny(t *testing.T) {
	composeUp(t, "../../examples/hello-shiny/docker-compose.yml")

	baseURL := "http://localhost:8080"
	dexURL := "http://localhost:5556"

	waitForHealth(t, baseURL, 60*time.Second)

	var (
		cookies []*http.Cookie
		token   string
	)

	t.Run("auth", func(t *testing.T) {
		cookies = dexLogin(t, baseURL, dexURL, dexEmail1, dexPassword)
		token = createPAT(t, baseURL, cookies)
		if !strings.HasPrefix(token, "by_") {
			t.Fatalf("token %q missing by_ prefix", token)
		}
	})

	t.Run("deploy", func(t *testing.T) {
		if token == "" {
			t.Skip("depends on auth")
		}

		appDir := copyAppDir(t, "../../examples/hello-shiny/app")

		var result map[string]any
		runCLIJSON(t, baseURL, token, &result,
			"deploy", appDir, "--yes", "--wait", "--name", "hello")

		if s, _ := result["status"].(string); s != "completed" {
			t.Fatalf("deploy status: got %q, want completed", s)
		}
		if s, _ := result["app"].(string); s != "hello" {
			t.Fatalf("deploy app: got %q, want hello", s)
		}
		if result["bundle_id"] == nil || result["bundle_id"] == "" {
			t.Fatal("deploy: missing bundle_id")
		}
	})

	t.Run("app_serves_html", func(t *testing.T) {
		if token == "" {
			t.Skip("depends on deploy")
		}

		// Ensure the app is enabled (should be by default after deploy).
		runCLI(t, baseURL, token, "enable", "hello")

		status, body := fetchAppPage(t, baseURL, "hello", cookies, 120*time.Second)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, "Hello Blockyard") {
			t.Fatalf("page body does not contain 'Hello Blockyard': %s", truncate(body, 500))
		}

		// Verify app is running via CLI.
		var app map[string]any
		runCLIJSON(t, baseURL, token, &app, "get", "hello")
		if s, _ := app["status"].(string); s != "running" {
			t.Fatalf("expected status running, got %q", s)
		}
	})

	t.Run("websocket_connects", func(t *testing.T) {
		if token == "" {
			t.Skip("depends on deploy")
		}

		dialAppWebSocket(t, baseURL, "hello", cookies)
	})

	t.Run("unauthenticated_redirects", func(t *testing.T) {
		if token == "" {
			t.Skip("depends on deploy")
		}

		status, _ := fetchAppPageNoRedirect(t, baseURL, "hello")
		if status != 302 && status != 303 {
			t.Fatalf("expected redirect (302/303), got %d", status)
		}
	})

	t.Run("cli_list_shows_app", func(t *testing.T) {
		if token == "" {
			t.Skip("depends on deploy")
		}

		var result struct {
			Apps []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"apps"`
		}
		runCLIJSON(t, baseURL, token, &result, "list")

		found := false
		for _, a := range result.Apps {
			if a.Name == "hello" {
				found = true
				if a.Status != "running" {
					t.Fatalf("list: app status = %q, want running", a.Status)
				}
				break
			}
		}
		if !found {
			t.Fatal("list: app 'hello' not found")
		}
	})

	t.Run("stop_and_cleanup", func(t *testing.T) {
		if token == "" {
			t.Skip("depends on deploy")
		}

		runCLI(t, baseURL, token, "disable", "hello")
		waitForAppStatus(t, baseURL, token, "hello", "stopped", 120*time.Second)

		runCLI(t, baseURL, token, "delete", "hello")

		// Verify the app is gone.
		runCLIFail(t, baseURL, token, "get", "hello", "--json")
	})
}
