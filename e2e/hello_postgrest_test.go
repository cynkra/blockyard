//go:build e2e

package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHelloPostgrest(t *testing.T) {
	composeUp(t, "../examples/hello-postgrest/docker-compose.yml")

	baseURL := "http://localhost:8080"
	dexURL := "http://localhost:5556"
	vaultURL := "http://localhost:8200"
	postgrestURL := "http://localhost:3001"

	waitForHealth(t, baseURL, 90*time.Second)

	var (
		cookies1 []*http.Cookie
		cookies2 []*http.Cookie
		token1   string
		appID    string
		client1  *APIClient
	)

	t.Run("vault_oidc_configured", func(t *testing.T) {
		// Verify the OIDC key and role exist in vault.
		req, _ := http.NewRequest("GET", vaultURL+"/v1/identity/oidc/role/postgrest", nil)
		req.Header.Set("X-Vault-Token", vaultRootToken)
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("read oidc role: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("oidc role: status %d, body: %s", resp.StatusCode, b)
		}

		// Verify JWKS endpoint is serving keys.
		jwksResp, err := http.Get(vaultURL + "/v1/identity/oidc/.well-known/keys")
		if err != nil {
			t.Fatalf("jwks: %v", err)
		}
		defer jwksResp.Body.Close()
		if jwksResp.StatusCode != http.StatusOK {
			t.Fatalf("jwks: status %d", jwksResp.StatusCode)
		}
		var jwks struct {
			Keys []any `json:"keys"`
		}
		json.NewDecoder(jwksResp.Body).Decode(&jwks)
		if len(jwks.Keys) == 0 {
			t.Fatal("jwks: no keys returned")
		}
	})

	t.Run("postgrest_reachable", func(t *testing.T) {
		// PostgREST should be running. Anonymous requests get 401
		// because the anon role has no permissions.
		resp, err := http.Get(postgrestURL + "/")
		if err != nil {
			t.Fatalf("postgrest: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			t.Fatalf("postgrest: server error %d", resp.StatusCode)
		}
	})

	t.Run("board_tables_exist", func(t *testing.T) {
		// The boards endpoint should exist (not 404). Without auth
		// we get 401, which proves the table is registered.
		resp, err := http.Get(postgrestURL + "/boards")
		if err != nil {
			t.Fatalf("boards endpoint: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Fatal("boards table not exposed — migration 004 may not have run")
		}
	})

	t.Run("user1_deploy", func(t *testing.T) {
		cookies1 = dexLogin(t, baseURL, dexURL, dexEmail1, dexPassword)
		token1 = createPAT(t, baseURL, cookies1)
		client1 = &APIClient{BaseURL: baseURL, Token: token1}

		appID = client1.CreateApp(t, "hello-postgrest")
		client1.UpdateApp(t, appID, `{"access_type":"logged_in","max_sessions_per_worker":10}`)

		bundle := makeBundle(t, "../examples/hello-postgrest/app")
		taskID, _ := client1.UploadBundle(t, appID, bundle)
		client1.PollTask(t, taskID, 10*time.Minute)

		client1.StartApp(t, appID)
	})

	t.Run("user1_app_serves", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on user1_deploy")
		}

		status, body := fetchAppPage(t, baseURL, "hello-postgrest", cookies1, 60*time.Second)
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

		dialAppWebSocket(t, baseURL, "hello-postgrest", cookies1)
	})

	t.Run("user2_access", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on user1_deploy")
		}

		cookies2 = dexLogin(t, baseURL, dexURL, dexEmail2, dexPassword)

		status, _ := fetchAppPage(t, baseURL, "hello-postgrest", cookies2, 60*time.Second)
		if status != 200 {
			t.Fatalf("user2 access: expected 200, got %d", status)
		}
	})

	t.Run("user2_websocket", func(t *testing.T) {
		if cookies2 == nil {
			t.Skip("depends on user2_access")
		}

		dialAppWebSocket(t, baseURL, "hello-postgrest", cookies2)
	})

	t.Run("stop_and_cleanup", func(t *testing.T) {
		if appID == "" || client1 == nil {
			t.Skip("depends on user1_deploy")
		}

		client1.StopApp(t, appID)
		client1.DeleteApp(t, appID)
	})
}
