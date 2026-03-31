//go:build ex

package ex_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// UI fetch helpers
// ---------------------------------------------------------------------------

// fetchPage performs an authenticated GET and returns status + body.
func fetchPage(t *testing.T, baseURL, path string, cookies []*http.Cookie) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// fetchPageNoAuth performs an unauthenticated GET without following redirects.
func fetchPageNoAuth(t *testing.T, baseURL, path string) (int, string, http.Header) {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(baseURL + path)
	if err != nil {
		t.Fatalf("GET %s (no auth): %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// patchForm sends a form-encoded PATCH (like htmx) and returns status + body.
func patchForm(t *testing.T, baseURL, path string, cookies []*http.Cookie, data url.Values) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("PATCH", baseURL+path, strings.NewReader(data.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// postForm sends a form-encoded POST and returns status + body.
func postForm(t *testing.T, baseURL, path string, cookies []*http.Cookie, data url.Values) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", baseURL+path, strings.NewReader(data.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// ---------------------------------------------------------------------------
// UI end-to-end tests
// ---------------------------------------------------------------------------

func TestHelloShinyUI(t *testing.T) {
	composeUp(t, "../examples/hello-shiny/docker-compose.yml")

	baseURL := "http://localhost:8080"
	dexURL := "http://localhost:5556"

	waitForHealth(t, baseURL, 60*time.Second)

	var (
		cookies []*http.Cookie
		token   string
		appID   string
	)

	// ---- Setup: auth + deploy ----

	t.Run("setup", func(t *testing.T) {
		cookies = dexLogin(t, baseURL, dexURL, dexEmail1, dexPassword)
		token = createPAT(t, baseURL, cookies)

		appDir := copyAppDir(t, "../examples/hello-shiny/app")
		var result map[string]any
		runCLIJSON(t, baseURL, token, &result,
			"deploy", appDir, "--yes", "--wait", "--name", "hello")
		if s, _ := result["status"].(string); s != "completed" {
			t.Fatalf("deploy status: got %q, want completed", s)
		}

		// Get app UUID for API calls.
		var app map[string]any
		runCLIJSON(t, baseURL, token, &app, "get", "hello")
		appID, _ = app["id"].(string)
		if appID == "" {
			t.Fatal("missing app ID")
		}
	})

	// ---- Apps catalog page ----

	t.Run("apps_page_shows_deployed_app", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, `class="app-card"`) {
			t.Fatal("no app-card found")
		}
		if !strings.Contains(body, ">hello</span>") {
			t.Fatal("app name 'hello' not in card")
		}
		if !strings.Contains(body, "stopped") {
			t.Fatal("stopped status not shown (cold-start: no workers until first request)")
		}
		if !strings.Contains(body, `class="app-card-gear"`) {
			t.Fatal("gear icon not shown for owner")
		}
	})

	t.Run("apps_page_search_match", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/?search=hello", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, ">hello</span>") {
			t.Fatal("search did not return the app")
		}
	})

	t.Run("apps_page_search_no_match", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/?search=nonexistent", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if strings.Contains(body, `class="app-card"`) {
			t.Fatal("unexpected app card for non-matching search")
		}
		if !strings.Contains(body, `class="empty-state"`) {
			t.Fatal("expected empty-state section")
		}
	})

	// ---- Left navigation ----

	t.Run("nav_links_and_active_state", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}

		// Verify nav links on apps page.
		_, body := fetchPage(t, baseURL, "/", cookies)
		for _, link := range []string{`href="/"`, `href="/deployments"`, `href="/profile"`} {
			if !strings.Contains(body, link) {
				t.Errorf("nav link %q not found", link)
			}
		}

		// Verify active page highlighting changes per page.
		pages := map[string]string{
			"/":            "apps",
			"/deployments": "deployments",
			"/profile":     "profile",
		}
		for path, page := range pages {
			_, b := fetchPage(t, baseURL, path, cookies)
			// The active link has both the class and the page-specific href.
			if !strings.Contains(b, page) {
				t.Errorf("page %s: expected %q in active highlight", path, page)
			}
		}
	})

	// ---- Sidebar ----

	t.Run("sidebar_loads_with_tabs", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/ui/apps/hello/sidebar", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, `class="sidebar-header"`) {
			t.Fatal("sidebar header not found")
		}
		if !strings.Contains(body, ">hello</h2>") {
			t.Fatal("app name not in sidebar header")
		}
		for _, tab := range []string{"Overview", "Settings", "Runtime", "Bundles", "Collaborators", "Logs"} {
			if !strings.Contains(body, tab) {
				t.Errorf("tab %q not found", tab)
			}
		}
		// Overview should be pre-rendered.
		if !strings.Contains(body, `class="overview-grid"`) {
			t.Fatal("overview not pre-rendered in sidebar")
		}
	})

	// ---- Tab fragments ----

	t.Run("tab_overview", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/ui/apps/hello/tab/overview", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, `class="overview-grid"`) {
			t.Fatal("overview-grid not found")
		}
		for _, marker := range []string{"Status", "Workers", "Activity", "Bundle"} {
			if !strings.Contains(body, marker) {
				t.Errorf("overview card %q not found", marker)
			}
		}
	})

	t.Run("tab_settings", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/ui/apps/hello/tab/settings", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, `class="settings-view"`) {
			t.Fatal("settings-view not found")
		}
		for _, field := range []string{
			`name="title"`, `name="description"`,
			`name="memory_limit"`, `name="cpu_limit"`,
			`name="max_workers_per_app"`, `name="max_sessions_per_worker"`,
			`name="pre_warmed_seats"`,
		} {
			if !strings.Contains(body, field) {
				t.Errorf("field %q not found", field)
			}
		}
		if !strings.Contains(body, `class="app-controls"`) {
			t.Fatal("app controls not found")
		}
		if !strings.Contains(body, `class="danger-zone"`) {
			t.Fatal("danger zone not found")
		}
	})

	t.Run("tab_runtime", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/ui/apps/hello/tab/runtime", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, `class="runtime-view"`) {
			t.Fatal("runtime-view not found")
		}
		if !strings.Contains(body, `class="runtime-summary"`) {
			t.Fatal("runtime summary not found")
		}
	})

	t.Run("tab_bundles", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/ui/apps/hello/tab/bundles", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, `class="bundles-view"`) {
			t.Fatal("bundles-view not found")
		}
		if !strings.Contains(body, "status-badge") {
			t.Fatal("no bundle status badge")
		}
		// Active bundle should be marked.
		if !strings.Contains(body, ">active</span>") {
			t.Fatal("active bundle label not found")
		}
	})

	t.Run("tab_collaborators", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/ui/apps/hello/tab/collaborators", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, `class="collaborators-view"`) {
			t.Fatal("collaborators-view not found")
		}
		if !strings.Contains(body, `name="access_type"`) {
			t.Fatal("access type selector not found")
		}
	})

	t.Run("tab_logs", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/ui/apps/hello/tab/logs", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, `class="log-viewer"`) {
			t.Fatal("log-viewer not found")
		}
		if !strings.Contains(body, `class="worker-list"`) {
			t.Fatal("worker list not found")
		}
	})

	// ---- Settings update round-trip ----

	t.Run("settings_update_description", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on setup")
		}
		data := url.Values{"description": {"E2E test description"}}
		status, _ := patchForm(t, baseURL, "/api/v1/apps/"+appID, cookies, data)
		if status != 200 {
			t.Fatalf("PATCH description: expected 200, got %d", status)
		}

		// Verify the change appears in the settings tab.
		_, body := fetchPage(t, baseURL, "/ui/apps/hello/tab/settings", cookies)
		if !strings.Contains(body, "E2E test description") {
			t.Fatal("updated description not reflected in settings tab")
		}
	})

	t.Run("settings_update_title", func(t *testing.T) {
		if appID == "" {
			t.Skip("depends on setup")
		}
		data := url.Values{"title": {"Hello World App"}}
		status, _ := patchForm(t, baseURL, "/api/v1/apps/"+appID, cookies, data)
		if status != 200 {
			t.Fatalf("PATCH title: expected 200, got %d", status)
		}

		// Title should appear on apps page card.
		_, body := fetchPage(t, baseURL, "/", cookies)
		if !strings.Contains(body, "Hello World App") {
			t.Fatal("updated title not reflected on apps page")
		}
	})

	// ---- Deployments page ----

	t.Run("deployments_page_shows_deploy", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/deployments", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, `class="data-table"`) {
			t.Fatal("data table not found")
		}
		if !strings.Contains(body, "hello") {
			t.Fatal("app 'hello' not in deployment history")
		}
		if !strings.Contains(body, "ready") {
			t.Fatal("ready status not found (bundle status after deploy is 'ready')")
		}
	})

	t.Run("deployments_page_search", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/deployments?search=hello", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, "hello") {
			t.Fatal("search did not return the deployment")
		}

		// Non-matching search should show empty state.
		_, noMatch := fetchPage(t, baseURL, "/deployments?search=nonexistent", cookies)
		if !strings.Contains(noMatch, "No deployments found") {
			t.Fatal("expected empty-state for non-matching search")
		}
	})

	// ---- Profile page ----

	t.Run("profile_page_renders", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, body := fetchPage(t, baseURL, "/profile", cookies)
		if status != 200 {
			t.Fatalf("expected 200, got %d", status)
		}
		if !strings.Contains(body, dexEmail1) {
			t.Fatal("user email not shown")
		}
		if !strings.Contains(body, `class="role-badge"`) {
			t.Fatal("role badge not found")
		}
		if !strings.Contains(body, `id="tokens"`) {
			t.Fatal("token section not found")
		}
		if !strings.Contains(body, `hx-post="/ui/tokens"`) {
			t.Fatal("token creation form not found")
		}
	})

	// ---- PAT creation via UI form ----

	t.Run("create_token_via_ui_form", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		data := url.Values{"name": {"e2e-ui-token"}}
		status, body := postForm(t, baseURL, "/ui/tokens", cookies, data)
		if status != 200 {
			t.Fatalf("POST /ui/tokens: expected 200, got %d", status)
		}
		if !strings.Contains(body, `class="pat-created"`) {
			t.Fatal("pat-created fragment not returned")
		}
		if !strings.Contains(body, "by_") {
			t.Fatal("token with by_ prefix not found in response")
		}
		// Token should also be visible by name if we reload the profile page.
		_, profile := fetchPage(t, baseURL, "/profile", cookies)
		if !strings.Contains(profile, "e2e-ui-token") {
			t.Fatal("created token not listed on profile page")
		}
	})

	// ---- Unauthenticated access ----

	t.Run("unauthenticated_pages_redirect", func(t *testing.T) {
		// These pages require auth and should redirect to /login.
		for _, path := range []string{"/deployments", "/profile"} {
			status, _, headers := fetchPageNoAuth(t, baseURL, path)
			if status != http.StatusFound {
				t.Errorf("GET %s: expected 302, got %d", path, status)
				continue
			}
			loc := headers.Get("Location")
			if !strings.Contains(loc, "/login") {
				t.Errorf("GET %s: expected redirect to /login, got %q", path, loc)
			}
		}
	})

	t.Run("unauthenticated_landing_page", func(t *testing.T) {
		// Root without auth shows landing page, not a redirect.
		status, body, _ := fetchPageNoAuth(t, baseURL, "/")
		if status != 200 {
			t.Fatalf("landing page: expected 200, got %d", status)
		}
		// Landing page should not have left nav.
		if strings.Contains(body, `class="left-nav"`) {
			t.Fatal("landing page should not have left nav")
		}
	})

	t.Run("sidebar_requires_auth", func(t *testing.T) {
		status, _, _ := fetchPageNoAuth(t, baseURL, "/ui/apps/hello/sidebar")
		if status != http.StatusNotFound {
			t.Fatalf("sidebar without auth: expected 404, got %d", status)
		}
	})

	t.Run("tabs_require_auth", func(t *testing.T) {
		tabs := []string{"overview", "settings", "runtime", "bundles", "collaborators", "logs"}
		for _, tab := range tabs {
			status, _, _ := fetchPageNoAuth(t, baseURL, "/ui/apps/hello/tab/"+tab)
			if status != http.StatusNotFound {
				t.Errorf("tab %s without auth: expected 404, got %d", tab, status)
			}
		}
	})

	t.Run("sidebar_404_nonexistent_app", func(t *testing.T) {
		if cookies == nil {
			t.Skip("depends on setup")
		}
		status, _ := fetchPage(t, baseURL, "/ui/apps/no-such-app/sidebar", cookies)
		if status != 404 {
			t.Fatalf("sidebar for nonexistent app: expected 404, got %d", status)
		}
	})

	t.Run("token_post_requires_auth", func(t *testing.T) {
		// POST without auth should return 401, not redirect.
		req, _ := http.NewRequest("POST", baseURL+"/ui/tokens",
			strings.NewReader(url.Values{"name": {"test"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("POST /ui/tokens: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	})

	// ---- Cleanup ----

	t.Run("cleanup", func(t *testing.T) {
		if token == "" {
			t.Skip("depends on setup")
		}
		runCLI(t, baseURL, token, "disable", "hello")
		waitForAppStatus(t, baseURL, token, "hello", "stopped", 120*time.Second)
		runCLI(t, baseURL, token, "delete", "hello")
	})
}
