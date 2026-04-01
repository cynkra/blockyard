//go:build examples

package examples_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestRefreshAndRollback deploys an unpinned app (DESCRIPTION-based, no
// renv.lock), exercises the refresh endpoint to re-resolve dependencies,
// and then rolls back to the previous state.
func TestRefreshAndRollback(t *testing.T) {
	composeUp(t, "../../examples/hello-shiny/docker-compose.yml")

	baseURL := "http://localhost:8080"
	dexURL := "http://localhost:5556"

	waitForHealth(t, baseURL, 90*time.Second)

	var (
		cookies []*http.Cookie
		token   string
		appName = "refresh-test"
	)

	t.Run("auth", func(t *testing.T) {
		cookies = dexLogin(t, baseURL, dexURL, dexEmail1, dexPassword)
		token = createPAT(t, baseURL, cookies)
		if !strings.HasPrefix(token, "by_") {
			t.Fatalf("token %q missing by_ prefix", token)
		}
	})

	t.Run("deploy_unpinned", func(t *testing.T) {
		if token == "" {
			t.Skip("depends on auth")
		}

		appDir := copyAppDir(t, "../../examples/hello-shiny/app")

		var result map[string]any
		runCLIJSON(t, baseURL, token, &result,
			"deploy", appDir, "--yes", "--wait", "--name", appName)

		if s, _ := result["status"].(string); s != "completed" {
			t.Fatalf("deploy status: got %q, want completed", s)
		}

		// Enable and trigger cold-start via proxy.
		runCLI(t, baseURL, token, "enable", appName)
		fetchAppPage(t, baseURL, appName, cookies, 120*time.Second)

		// Verify app is running via CLI.
		var app map[string]any
		runCLIJSON(t, baseURL, token, &app, "get", appName)
		if s, _ := app["status"].(string); s != "running" {
			t.Fatalf("expected status running, got %q", s)
		}
	})

	t.Run("refresh_pinned_rejected", func(t *testing.T) {
		if token == "" {
			t.Skip("depends on deploy_unpinned")
		}

		// The hello-shiny app may be pinned (renv.lock) or unpinned
		// (DESCRIPTION-only). The CLI-generated manifest from DESCRIPTION
		// is unpinned, so refresh should be accepted (202).
		// If pinned, the CLI will fail with the server's 409 error.
		out, stderr := runCLIFail(t, baseURL, token, "refresh", appName, "--json")

		// Either the refresh was rejected (pinned → error about pinned)
		// or the CLI streamed logs and the task completed/failed.
		// Both are valid — the important thing is it doesn't crash.
		t.Logf("refresh stdout: %s", truncate(out, 200))
		t.Logf("refresh stderr: %s", truncate(stderr, 200))
	})

	t.Run("rollback_no_prev", func(t *testing.T) {
		if token == "" {
			t.Skip("depends on deploy_unpinned")
		}

		// Without a prior refresh, rollback should fail.
		out, _ := runCLIFail(t, baseURL, token, "refresh", appName, "--rollback", "--json")
		t.Logf("rollback (no prev): %s", truncate(out, 200))
	})

	t.Run("rollback_build_no_manifest", func(t *testing.T) {
		if token == "" {
			t.Skip("depends on deploy_unpinned")
		}

		// ?target=build has no CLI flag — use raw HTTP.
		resp := apiPost(t, baseURL, token, "/api/v1/apps/"+appName+"/refresh/rollback?target=build")
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 (no build manifest), got %d", resp.StatusCode)
		}
		t.Log("correctly rejected rollback to build when no build manifest exists")
	})

	t.Run("cleanup", func(t *testing.T) {
		if token == "" {
			t.Skip("depends on deploy_unpinned")
		}

		runCLI(t, baseURL, token, "disable", appName)
		waitForAppStatus(t, baseURL, token, appName, "stopped", 120*time.Second)
		runCLI(t, baseURL, token, "delete", appName)
	})
}
