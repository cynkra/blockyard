//go:build examples

package examples_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHelloPostgrest(t *testing.T) {
	// Skipped per #283 ACs: the board-storage control plane moved
	// off PostgREST, so this example's runtime is broken end-to-end
	// (migration 007 drops public.boards, PostgREST returns 404 for
	// /boards). The example itself is replaced by hello-postgres
	// (direct PG + vault creds) in #285; this test is either
	// rewritten or removed alongside it.
	t.Skip("hello-postgrest example is broken after #283; rewritten in #285")

	composeUp(t, "../../examples/hello-postgrest/docker-compose.yml")

	baseURL := "http://localhost:8080"
	dexURL := "http://localhost:5556"
	vaultURL := "http://localhost:8200"
	postgrestURL := "http://localhost:3001"

	waitForHealth(t, baseURL, 90*time.Second)

	var (
		client1 *http.Client
		client2 *http.Client
		token1  string
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
		cookies1 := dexLogin(t, baseURL, dexURL, dexEmail1, dexPassword)
		token1 = createPAT(t, baseURL, cookies1)
		client1 = newProxyClient(t, baseURL, cookies1)

		appDir := copyAppDir(t, "../../examples/hello-postgrest/app")

		var result map[string]any
		runCLIJSON(t, baseURL, token1, &result,
			"deploy", appDir, "--yes", "--wait", "--name", "hello-postgrest")

		if s, _ := result["status"].(string); s != "completed" {
			t.Fatalf("deploy status: got %q, want completed", s)
		}

		// Configure access and scaling via CLI.
		runCLI(t, baseURL, token1, "access", "set-type", "hello-postgrest", "logged_in")
		runCLI(t, baseURL, token1, "scale", "hello-postgrest", "--max-sessions", "10")

		// Enable and trigger cold-start via proxy.
		runCLI(t, baseURL, token1, "enable", "hello-postgrest")
		fetchAppPage(t, client1, baseURL, "hello-postgrest", 120*time.Second)
	})

	t.Run("user1_app_serves", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}

		status, body := fetchAppPage(t, client1, baseURL, "hello-postgrest", 60*time.Second)
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

		dialAppWebSocket(t, client1, baseURL, "hello-postgrest")
	})

	t.Run("user2_access", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}

		cookies2 := dexLogin(t, baseURL, dexURL, dexEmail2, dexPassword)
		client2 = newProxyClient(t, baseURL, cookies2)

		status, _ := fetchAppPage(t, client2, baseURL, "hello-postgrest", 60*time.Second)
		if status != 200 {
			t.Fatalf("user2 access: expected 200, got %d", status)
		}
	})

	t.Run("user2_websocket", func(t *testing.T) {
		if client2 == nil {
			t.Skip("depends on user2_access")
		}

		dialAppWebSocket(t, client2, baseURL, "hello-postgrest")
	})

	t.Run("stop_and_cleanup", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}

		runCLI(t, baseURL, token1, "disable", "hello-postgrest")
		waitForAppStatus(t, baseURL, token1, "hello-postgrest", "stopped", 120*time.Second)
		runCLI(t, baseURL, token1, "delete", "hello-postgrest")
	})
}
