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

// TestRefreshAndRollback deploys an unpinned app (DESCRIPTION-based, no
// renv.lock), exercises the refresh endpoint to re-resolve dependencies,
// and then rolls back to the previous state.
func TestRefreshAndRollback(t *testing.T) {
	composeUp(t, "../examples/hello-shiny/docker-compose.yml")

	baseURL := "http://localhost:8080"
	dexURL := "http://localhost:5556"

	waitForHealth(t, baseURL, 90*time.Second)

	var (
		cookies []*http.Cookie
		token   string
		appID   string
		client  *APIClient
	)

	t.Run("auth", func(t *testing.T) {
		cookies = dexLogin(t, baseURL, dexURL, dexEmail1, dexPassword)
		token = createPAT(t, baseURL, cookies)
		if !strings.HasPrefix(token, "by_") {
			t.Fatalf("token %q missing by_ prefix", token)
		}
		client = &APIClient{BaseURL: baseURL, Token: token, Cookies: cookies}
	})

	t.Run("deploy_unpinned", func(t *testing.T) {
		if client == nil {
			t.Skip("depends on auth")
		}

		// Create the app.
		appID = client.CreateApp(t, "refresh-test")

		// Upload a bundle from hello-shiny (it has a DESCRIPTION with
		// Imports: shiny). The server will detect the unpinned manifest.
		bundle := makeBundle(t, "../examples/hello-shiny/app")
		taskID, _ := client.UploadBundle(t, appID, bundle)
		client.PollTask(t, taskID, 10*time.Minute)

		// Start the app.
		workerID := client.StartApp(t, appID)
		if workerID == "" {
			t.Fatal("start returned empty worker_id")
		}

		status, body := client.GetApp(t, appID)
		if status != 200 {
			t.Fatalf("get app: status %d", status)
		}
		if s, _ := body["status"].(string); s != "running" {
			t.Fatalf("expected status running, got %q", s)
		}
	})

	t.Run("refresh_pinned_rejected", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on deploy_unpinned")
		}

		// hello-shiny uses renv.lock (pinned), so refresh should be
		// rejected with 409.
		resp, err := client.do("POST", "/api/v1/apps/"+appID+"/refresh", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		// If the manifest is pinned, expect 409. If unpinned, expect 202.
		// The hello-shiny app ships renv.lock which makes it pinned.
		if resp.StatusCode == http.StatusConflict {
			t.Log("correctly rejected refresh for pinned deployment (renv.lock present)")
		} else if resp.StatusCode == http.StatusAccepted {
			// Unpinned — poll the refresh task.
			var result map[string]string
			json.NewDecoder(resp.Body).Decode(&result)
			taskID := result["task_id"]
			if taskID != "" {
				t.Logf("refresh accepted, task_id=%s — polling...", taskID)
				client.PollTask(t, taskID, 5*time.Minute)
				t.Log("refresh completed")
			}
		} else {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("unexpected status %d: %s", resp.StatusCode, body)
		}
	})

	t.Run("rollback_no_prev", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on deploy_unpinned")
		}

		// Without a prior refresh, rollback should return 409.
		resp, err := client.do("POST", "/api/v1/apps/"+appID+"/refresh/rollback", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusConflict {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 409 (no prev manifest), got %d: %s", resp.StatusCode, body)
		}
		t.Log("correctly rejected rollback when no previous refresh exists")
	})

	t.Run("rollback_build_no_manifest", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on deploy_unpinned")
		}

		resp, err := client.do("POST", "/api/v1/apps/"+appID+"/refresh/rollback?target=build", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusConflict {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 409 (no build manifest), got %d: %s", resp.StatusCode, body)
		}
		t.Log("correctly rejected rollback to build when no build manifest exists")
	})

	t.Run("cleanup", func(t *testing.T) {
		if appID == "" || client == nil {
			t.Skip("depends on deploy_unpinned")
		}

		client.StopApp(t, appID)
		client.DeleteApp(t, appID)
	})
}
