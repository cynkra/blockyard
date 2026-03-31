//go:build examples

package examples_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHelloPocketbase(t *testing.T) {
	composeUp(t, "../examples/hello-pocketbase/docker-compose.yml")

	baseURL := "http://localhost:8080"
	dexURL := "http://localhost:5556"
	vaultURL := "http://localhost:8200"

	waitForHealth(t, baseURL, 90*time.Second)

	var (
		cookies1 []*http.Cookie
		cookies2 []*http.Cookie
		token1   string
	)

	t.Run("pre_enrolled_credentials", func(t *testing.T) {
		// Read demo user's pocketbase credential directly from OpenBao.
		data1 := readVaultSecret(t, vaultURL, vaultRootToken,
			"secret/data/users/"+demoSub1+"/apikeys/pocketbase")
		if email, _ := data1["email"].(string); email != dexEmail1 {
			t.Fatalf("demo1 pocketbase email: got %q, want %q", email, dexEmail1)
		}

		data2 := readVaultSecret(t, vaultURL, vaultRootToken,
			"secret/data/users/"+demoSub2+"/apikeys/pocketbase")
		if email, _ := data2["email"].(string); email != dexEmail2 {
			t.Fatalf("demo2 pocketbase email: got %q, want %q", email, dexEmail2)
		}
	})

	t.Run("user1_deploy", func(t *testing.T) {
		cookies1 = dexLogin(t, baseURL, dexURL, dexEmail1, dexPassword)
		token1 = createPAT(t, baseURL, cookies1)

		// Set access_type and scaling before deploy starts the app.
		appDir := copyAppDir(t, "../examples/hello-pocketbase/app")

		var result map[string]any
		runCLIJSON(t, baseURL, token1, &result,
			"deploy", appDir, "--yes", "--wait", "--name", "hello-pocketbase")

		if s, _ := result["status"].(string); s != "completed" {
			t.Fatalf("deploy status: got %q, want completed", s)
		}

		// Configure access and scaling via CLI.
		runCLI(t, baseURL, token1, "access", "set-type", "hello-pocketbase", "logged_in")
		runCLI(t, baseURL, token1, "scale", "hello-pocketbase", "--max-sessions", "10")

		// Enable and trigger cold-start via proxy.
		runCLI(t, baseURL, token1, "enable", "hello-pocketbase")
		fetchAppPage(t, baseURL, "hello-pocketbase", cookies1, 120*time.Second)
	})

	t.Run("user1_app_serves", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}

		status, body := fetchAppPage(t, baseURL, "hello-pocketbase", cookies1, 60*time.Second)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, "<html") && !strings.Contains(body, "<HTML") {
			t.Fatalf("page body does not contain <html: %s", truncate(body, 500))
		}
	})

	t.Run("user1_websocket", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}

		dialAppWebSocket(t, baseURL, "hello-pocketbase", cookies1)
	})

	t.Run("user2_access", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}

		// User2 logs in — different session.
		cookies2 = dexLogin(t, baseURL, dexURL, dexEmail2, dexPassword)

		// User2 should be able to access the app (access_type=logged_in).
		status, _ := fetchAppPage(t, baseURL, "hello-pocketbase", cookies2, 60*time.Second)
		if status != 200 {
			t.Fatalf("user2 access: expected 200, got %d", status)
		}
	})

	t.Run("user2_websocket", func(t *testing.T) {
		if cookies2 == nil {
			t.Skip("depends on user2_access")
		}

		dialAppWebSocket(t, baseURL, "hello-pocketbase", cookies2)
	})

	t.Run("enroll_credential_via_api", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}

		enrollCredentialWithPAT(t, baseURL, token1, "test-service", "sk-test-12345")

		// Verify in OpenBao.
		data := readVaultSecret(t, vaultURL, vaultRootToken,
			"secret/data/users/"+demoSub1+"/apikeys/test-service")
		if key, _ := data["api_key"].(string); key != "sk-test-12345" {
			t.Fatalf("enrolled credential: got api_key=%q, want %q", key, "sk-test-12345")
		}
	})

	t.Run("user2_cannot_manage", func(t *testing.T) {
		if token1 == "" || cookies2 == nil {
			t.Skip("depends on user1_deploy and user2_access")
		}

		// User2 creates a PAT and tries to delete user1's app via CLI.
		token2 := createPAT(t, baseURL, cookies2)
		runCLIFail(t, baseURL, token2, "delete", "hello-pocketbase", "--json")
	})

	t.Run("stop_and_cleanup", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}

		runCLI(t, baseURL, token1, "disable", "hello-pocketbase")
		waitForAppStatus(t, baseURL, token1, "hello-pocketbase", "stopped", 120*time.Second)
		runCLI(t, baseURL, token1, "delete", "hello-pocketbase")
	})
}
