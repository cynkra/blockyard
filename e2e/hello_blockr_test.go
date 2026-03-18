//go:build e2e

package e2e_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHelloBlockr(t *testing.T) {
	composeUp(t, "../examples/hello-blockr/docker-compose.yml")

	baseURL := "http://localhost:8080"
	dexURL := "http://localhost:5556"
	vaultURL := "http://localhost:8200"

	waitForHealth(t, baseURL, 90*time.Second)

	var (
		cookies1 []*http.Cookie
		cookies2 []*http.Cookie
		token1   string
		appID    string
		client1  *APIClient
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
		client1 = &APIClient{BaseURL: baseURL, Token: token1}

		appID = client1.CreateApp(t, "hello-blockr")
		client1.UpdateApp(t, appID, `{"access_type":"logged_in"}`)

		bundle := makeBundle(t, "../examples/hello-blockr/app")
		taskID, _ := client1.UploadBundle(t, appID, bundle)
		client1.PollTask(t, taskID, 10*time.Minute)

		client1.StartApp(t, appID)
	})

	t.Run("user1_app_serves", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on user1_deploy")
		}

		status, body := fetchAppPage(t, baseURL, "hello-blockr", cookies1, 60*time.Second)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, "<html") && !strings.Contains(body, "<HTML") {
			t.Fatalf("page body does not contain <html: %s", truncate(body, 500))
		}
	})

	t.Run("user1_websocket", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on user1_deploy")
		}

		dialAppWebSocket(t, baseURL, "hello-blockr", cookies1)
	})

	t.Run("user2_access", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on user1_deploy")
		}

		// User2 logs in — different session.
		cookies2 = dexLogin(t, baseURL, dexURL, dexEmail2, dexPassword)

		// User2 should be able to access the app (access_type=logged_in).
		status, _ := fetchAppPage(t, baseURL, "hello-blockr", cookies2, 60*time.Second)
		if status != 200 {
			t.Fatalf("user2 access: expected 200, got %d", status)
		}
	})

	t.Run("user2_websocket", func(t *testing.T) {
		if cookies2 == nil {
			t.Skip("depends on user2_access")
		}

		dialAppWebSocket(t, baseURL, "hello-blockr", cookies2)
	})

	t.Run("enroll_credential_via_api", func(t *testing.T) {
		if cookies1 == nil {
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
		if appID == "" || cookies2 == nil {
			t.Skip("depends on user1_deploy and user2_access")
		}

		// User2 creates a PAT.
		token2 := createPAT(t, baseURL, cookies2)
		client2 := &APIClient{BaseURL: baseURL, Token: token2}

		// User2 tries to delete user1's app — should get 404 (not owner).
		status := client2.DeleteAppRaw(appID)
		if status != 404 {
			t.Fatalf("user2 delete app: expected 404, got %d", status)
		}
	})

	t.Run("stop_and_cleanup", func(t *testing.T) {
		if appID == "" || client1 == nil {
			t.Skip("depends on user1_deploy")
		}

		client1.StopApp(t, appID)
		client1.DeleteApp(t, appID)
	})
}
