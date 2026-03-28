//go:build e2e

package e2e_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHelloShiny(t *testing.T) {
	composeUp(t, "../examples/hello-shiny/docker-compose.yml")

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

		appDir := copyAppDir(t, "../examples/hello-shiny/app")

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
