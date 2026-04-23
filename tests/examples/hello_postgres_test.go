//go:build examples

package examples_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestHelloPostgres exercises the hello-postgres example end-to-end:
// docker compose up → OIDC login for two users → per-user PG role
// gets provisioned in Postgres, registered with vault's DB secrets
// engine, and recorded on blockyard.users.pg_role → app serves over
// the proxy and accepts a websocket upgrade.
//
// The policy-boundary probe runs against the live stack to confirm
// the security invariant documented in the example README:
// blockyard's own token cannot read any user's DB creds.
func TestHelloPostgres(t *testing.T) {
	composeUp(t, "../../examples/hello-postgres/docker-compose.yml")

	baseURL := "http://localhost:8080"
	dexURL := "http://localhost:5556"
	vaultURL := "http://localhost:8200"

	waitForHealth(t, baseURL, 90*time.Second)

	var (
		cookies1 []*http.Cookie
		cookies2 []*http.Cookie
		client1  *http.Client
		client2  *http.Client
		token1   string
	)

	t.Run("vault_db_engine_configured", func(t *testing.T) {
		// The DB secrets engine is mounted at `database/` and the
		// connection blockyard targets is named `blockyard`; both are
		// load-bearing because blockyard's toml points at them.
		req, _ := http.NewRequest("GET", vaultURL+"/v1/sys/mounts", nil)
		req.Header.Set("X-Vault-Token", vaultRootToken)
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("sys/mounts: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("sys/mounts: status %d, body: %s", resp.StatusCode, b)
		}
		var mounts map[string]any
		json.NewDecoder(resp.Body).Decode(&mounts)
		if _, ok := mounts["database/"]; !ok {
			t.Fatal("database/ mount missing from vault")
		}

		// Connection exists.
		connReq, _ := http.NewRequest("GET", vaultURL+"/v1/database/config/blockyard", nil)
		connReq.Header.Set("X-Vault-Token", vaultRootToken)
		connResp, err := httpClient.Do(connReq)
		if err != nil {
			t.Fatalf("database/config/blockyard: %v", err)
		}
		connResp.Body.Close()
		if connResp.StatusCode != http.StatusOK {
			t.Fatalf("database/config/blockyard: status %d", connResp.StatusCode)
		}
	})

	t.Run("user1_deploy", func(t *testing.T) {
		cookies1 = dexLogin(t, baseURL, dexURL, dexEmail1, dexPassword)
		token1 = createPAT(t, baseURL, cookies1)
		client1 = newProxyClient(t, baseURL, cookies1)

		appDir := copyAppDir(t, "../../examples/hello-postgres/app")

		var result map[string]any
		runCLIJSON(t, baseURL, token1, &result,
			"deploy", appDir, "--yes", "--wait", "--name", "hello-postgres")

		if s, _ := result["status"].(string); s != "completed" {
			t.Fatalf("deploy status: got %q, want completed", s)
		}

		runCLI(t, baseURL, token1, "access", "set-type", "hello-postgres", "logged_in")
		runCLI(t, baseURL, token1, "scale", "hello-postgres", "--max-sessions", "10")
		runCLI(t, baseURL, token1, "enable", "hello-postgres")
		fetchAppPage(t, client1, baseURL, "hello-postgres", 120*time.Second)
	})

	t.Run("user1_pg_role_provisioned", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}
		// First login should have populated users.pg_role. Read it
		// via blockyard's admin DB connection to avoid racing on any
		// timing assumptions about when provisioning fires.
		role := readPgRoleForSub(t, demoSub1)
		if role == "" {
			t.Fatal("users.pg_role for demo user 1 was not populated")
		}
		if !strings.HasPrefix(role, "user_") {
			t.Fatalf("pg_role %q does not start with user_", role)
		}

		// vault has a matching static-role entry; read it as root.
		staticRolePath := fmt.Sprintf("/v1/database/static-roles/%s", role)
		req, _ := http.NewRequest("GET", vaultURL+staticRolePath, nil)
		req.Header.Set("X-Vault-Token", vaultRootToken)
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("static-role read: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("static-role %s: status %d", role, resp.StatusCode)
		}
	})

	t.Run("user1_app_serves", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}
		status, body := fetchAppPage(t, client1, baseURL, "hello-postgres", 60*time.Second)
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
		dialAppWebSocket(t, client1, baseURL, "hello-postgres")
	})

	t.Run("user2_access_and_provision", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}
		cookies2 = dexLogin(t, baseURL, dexURL, dexEmail2, dexPassword)
		client2 = newProxyClient(t, baseURL, cookies2)

		status, _ := fetchAppPage(t, client2, baseURL, "hello-postgres", 60*time.Second)
		if status != 200 {
			t.Fatalf("user2 access: expected 200, got %d", status)
		}

		// User 2 also got a distinct PG role provisioned.
		role2 := readPgRoleForSub(t, demoSub2)
		role1 := readPgRoleForSub(t, demoSub1)
		if role2 == "" {
			t.Fatal("users.pg_role for demo user 2 was not populated")
		}
		if role1 != "" && role1 == role2 {
			t.Fatalf("demo users ended up with the same pg_role %q", role1)
		}
	})

	t.Run("user2_websocket", func(t *testing.T) {
		if client2 == nil {
			t.Skip("depends on user2_access_and_provision")
		}
		dialAppWebSocket(t, client2, baseURL, "hello-postgres")
	})

	t.Run("blockyard_token_cannot_read_user_creds", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}
		// Log in as the blockyard-server AppRole and confirm its
		// token is denied access to a provisioned user's static-creds
		// path. This is the load-bearing policy boundary from #285:
		// blockyard can define user_* roles but cannot mint their creds.
		appToken := appRoleLogin(t, vaultURL,
			"blockyard-server", "dev-secret-id-for-local-use-only")

		role := readPgRoleForSub(t, demoSub1)
		if role == "" {
			t.Skip("demo1 pg_role not populated — provisioning didn't run")
		}

		req, _ := http.NewRequest("GET",
			vaultURL+"/v1/database/static-creds/"+role, nil)
		req.Header.Set("X-Vault-Token", appToken)
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("static-creds probe: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("blockyard token read of user static-creds: got %d, want 403",
				resp.StatusCode)
		}
	})

	t.Run("stop_and_cleanup", func(t *testing.T) {
		if token1 == "" {
			t.Skip("depends on user1_deploy")
		}
		runCLI(t, baseURL, token1, "disable", "hello-postgres")
		waitForAppStatus(t, baseURL, token1, "hello-postgres", "stopped", 120*time.Second)
		runCLI(t, baseURL, token1, "delete", "hello-postgres")
	})
}

// readPgRoleForSub reads users.pg_role from the blockyard database
// by shelling into the postgres container. Uses the root DB user
// because the AppRole-managed admin credential path would require
// plumbing we don't need for a single read.
func readPgRoleForSub(t *testing.T, sub string) string {
	t.Helper()
	q := fmt.Sprintf(
		`SELECT COALESCE(pg_role, '') FROM blockyard.users WHERE sub = '%s'`,
		sub,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx,
		"docker", "compose",
		"-f", "../../examples/hello-postgres/docker-compose.yml",
		"exec", "-T", "postgres",
		"psql", "-U", "blockyard", "-d", "blockyard",
		"-t", "-A", "-c", q,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("readPgRoleForSub: %v\nstderr: %s", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// appRoleLogin exchanges a (role_id, secret_id) pair for a vault
// token. Mirrors what blockyard itself does at startup.
func appRoleLogin(t *testing.T, vaultURL, roleID, secretID string) string {
	t.Helper()
	body := fmt.Sprintf(`{"role_id":%q,"secret_id":%q}`, roleID, secretID)
	req, _ := http.NewRequest("POST",
		vaultURL+"/v1/auth/approle/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("approle login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("approle login: status %d, body: %s", resp.StatusCode, b)
	}
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Auth.ClientToken == "" {
		t.Fatal("approle login: empty client_token")
	}
	return out.Auth.ClientToken
}
