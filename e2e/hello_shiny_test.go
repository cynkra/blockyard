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
		appID   string
		client  *APIClient
	)

	t.Run("auth_and_pat", func(t *testing.T) {
		cookies = dexLogin(t, baseURL, dexURL, dexEmail1, dexPassword)
		token = createPAT(t, baseURL, cookies)
		if !strings.HasPrefix(token, "by_") {
			t.Fatalf("token %q missing by_ prefix", token)
		}
		client = &APIClient{BaseURL: baseURL, Token: token}
	})

	t.Run("deploy_app", func(t *testing.T) {
		if client == nil {
			t.Skip("depends on auth_and_pat")
		}

		appID = client.CreateApp(t, "hello")

		bundle := makeBundle(t, "../examples/hello-shiny/app")
		taskID, _ := client.UploadBundle(t, appID, bundle)
		client.PollTask(t, taskID, 10*time.Minute)

		workerID := client.StartApp(t, appID)
		if workerID == "" {
			t.Fatal("start returned empty worker_id")
		}

		// Verify app status is running.
		status, body := client.GetApp(t, appID)
		if status != 200 {
			t.Fatalf("get app: status %d", status)
		}
		if s, _ := body["status"].(string); s != "running" {
			t.Fatalf("expected status running, got %q", s)
		}
	})

	t.Run("app_serves_html", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on deploy_app")
		}

		status, body := fetchAppPage(t, baseURL, "hello", cookies, 60*time.Second)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, "Hello Blockyard") {
			t.Fatalf("page body does not contain 'Hello Blockyard': %s", truncate(body, 500))
		}
	})

	t.Run("websocket_connects", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on deploy_app")
		}

		dialAppWebSocket(t, baseURL, "hello", cookies)
	})

	t.Run("unauthenticated_redirects", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on deploy_app")
		}

		status, _ := fetchAppPageNoRedirect(t, baseURL, "hello")
		if status != 302 && status != 303 {
			t.Fatalf("expected redirect (302/303), got %d", status)
		}
	})

	t.Run("stop_and_cleanup", func(t *testing.T) {
		if appID == "" || client == nil {
			t.Skip("depends on deploy_app")
		}

		client.StopApp(t, appID)

		// Verify stopped.
		status, body := client.GetApp(t, appID)
		if status != 200 {
			t.Fatalf("get app after stop: status %d", status)
		}
		if s, _ := body["status"].(string); s != "stopped" {
			t.Fatalf("expected status stopped, got %q", s)
		}

		client.DeleteApp(t, appID)

		// Verify 404.
		getStatus, _ := client.GetApp(t, appID)
		if getStatus != 404 {
			t.Fatalf("expected 404 after delete, got %d", getStatus)
		}
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
