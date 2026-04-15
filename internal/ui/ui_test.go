package ui

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/integration"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
)

// --- Test helpers ---

// newTestServer creates a minimal server without auth context (unauthenticated).
func newTestServer(t *testing.T, cfg *config.Config) (*server.Server, *httptest.Server) {
	t.Helper()
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	// Track background restore goroutines so cleanup waits for them
	// before t.TempDir / DB teardown — otherwise the restore can race
	// with dir removal and DB close (see #234).
	var wg sync.WaitGroup
	srv.RestoreWG = &wg

	r := chi.NewRouter()
	uiHandler := New()
	uiHandler.RegisterRoutes(r, srv)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	// LIFO order: wg.Wait runs before ts.Close, DB close, TempDir cleanup.
	t.Cleanup(wg.Wait)
	return srv, ts
}

// authServer creates a test server with injected user/caller on all routes.
func authServer(t *testing.T, cfg *config.Config, sub string, role auth.Role) (*server.Server, *httptest.Server) {
	t.Helper()
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	// Ensure user exists in DB for profile lookups.
	database.UpsertUserWithRole(sub, sub+"@test.com", sub, role.String())

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	// Track background restore goroutines so cleanup waits for them
	// before t.TempDir / DB teardown — otherwise the restore can race
	// with dir removal and DB close (see #234).
	var wg sync.WaitGroup
	srv.RestoreWG = &wg

	uiHandler := New()
	r := chi.NewRouter()

	// Inject auth context on all requests.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := auth.ContextWithUser(req.Context(), &auth.AuthenticatedUser{
				Sub: sub,
			})
			ctx = auth.ContextWithCaller(ctx, &auth.CallerIdentity{
				Sub:    sub,
				Name:   sub,
				Role:   role,
				Source: auth.AuthSourceSession,
			})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})

	uiHandler.RegisterRoutes(r, srv)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	// LIFO order: wg.Wait runs before ts.Close, DB close, TempDir cleanup.
	t.Cleanup(wg.Wait)
	return srv, ts
}

func defaultConfig() *config.Config {
	return &config.Config{
		Server:  config.ServerConfig{},
		Docker:  config.DockerConfig{Image: "test-image", ShinyPort: 3838},
		Storage: config.StorageConfig{BundleServerPath: "/tmp", BundleWorkerPath: "/app"},
		Proxy:   config.ProxyConfig{MaxWorkers: 100},
	}
}

func oidcConfig() *config.Config {
	cfg := defaultConfig()
	cfg.OIDC = &config.OidcConfig{
		IssuerURL:    "http://localhost:9999",
		ClientID:     "test",
		ClientSecret: config.NewSecret("secret"),
	}
	cfg.Server.SessionSecret = secretPtr("session-secret")
	return cfg
}

func openbaoConfig() *config.Config {
	cfg := oidcConfig()
	cfg.Openbao = &config.OpenbaoConfig{
		Address:    "http://localhost:8200",
		AdminToken: config.NewSecret("root"),
		Services: []config.ServiceConfig{
			{ID: "openai", Label: "OpenAI"},
		},
	}
	return cfg
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func secretPtr(s string) *config.Secret {
	sec := config.NewSecret(s)
	return &sec
}

// noRedirectClient returns an HTTP client that does not follow redirects.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// --- Template and static tests ---

func TestNewDoesNotPanic(t *testing.T) {
	ui := New()
	if ui.pages == nil {
		t.Fatal("pages should not be nil")
	}
	if ui.fragments == nil {
		t.Fatal("fragments should not be nil")
	}
	if ui.static == nil {
		t.Fatal("static handler should not be nil")
	}
}

func TestEmbedContainsExpectedFiles(t *testing.T) {
	for _, path := range []string{
		"templates/base.html",
		"templates/landing.html",
		"templates/apps.html",
		"templates/deployments.html",
		"templates/api_keys.html",
		"templates/profile.html",
		"templates/pat_created.html",
		"static/style.css",
		"static/htmx.min.js",
	} {
		f, err := content.Open(path)
		if err != nil {
			t.Errorf("expected embedded file %q, got error: %v", path, err)
			continue
		}
		f.Close()
	}
}

func TestStaticCSS(t *testing.T) {
	_, ts := newTestServer(t, defaultConfig())

	resp, err := http.Get(ts.URL + "/static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/css") {
		t.Errorf("expected Content-Type containing text/css, got %q", ct)
	}
}

func TestStaticHtmx(t *testing.T) {
	_, ts := newTestServer(t, defaultConfig())

	resp, err := http.Get(ts.URL + "/static/htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// --- Template function tests ---

func TestDerefNilReturnsEmpty(t *testing.T) {
	fn := funcMap["deref"].(func(*string) string)
	if got := fn(nil); got != "" {
		t.Errorf("deref(nil) = %q, want empty", got)
	}
}

func TestDerefNonNil(t *testing.T) {
	fn := funcMap["deref"].(func(*string) string)
	s := "hello"
	if got := fn(&s); got != "hello" {
		t.Errorf("deref(&%q) = %q, want %q", s, got, s)
	}
}

func TestTruncateShort(t *testing.T) {
	fn := funcMap["truncate"].(func(string) string)
	if got := fn("abcd"); got != "abcd" {
		t.Errorf("truncate(%q) = %q, want %q", "abcd", got, "abcd")
	}
}

func TestTruncateLong(t *testing.T) {
	fn := funcMap["truncate"].(func(string) string)
	input := "abcdefghijklmnop"
	want := "abcdefgh..."
	if got := fn(input); got != want {
		t.Errorf("truncate(%q) = %q, want %q", input, got, want)
	}
}

func TestAddSubtract(t *testing.T) {
	add := funcMap["add"].(func(int, int) int)
	sub := funcMap["subtract"].(func(int, int) int)
	if got := add(3, 4); got != 7 {
		t.Errorf("add(3,4) = %d, want 7", got)
	}
	if got := sub(10, 3); got != 7 {
		t.Errorf("subtract(10,3) = %d, want 7", got)
	}
}

func TestTimeAgoNil(t *testing.T) {
	fn := funcMap["timeAgo"].(func(any) string)
	if got := fn(nil); got != "" {
		t.Errorf("timeAgo(nil) = %q, want empty", got)
	}
}

func TestTimeAgoParseError(t *testing.T) {
	fn := funcMap["timeAgo"].(func(any) string)
	bad := "not-a-date"
	if got := fn(&bad); got != bad {
		t.Errorf("timeAgo(invalid) = %q, want %q (passthrough)", got, bad)
	}
}

func TestTimeAgoSingularForms(t *testing.T) {
	fn := funcMap["timeAgo"].(func(any) string)

	cases := []struct {
		offset time.Duration
		want   string
	}{
		{90 * time.Second, "1 minute ago"},
		{90 * time.Minute, "1 hour ago"},
		{36 * time.Hour, "1 day ago"},
	}
	for _, tc := range cases {
		ts := time.Now().Add(-tc.offset).UTC().Format(time.RFC3339)
		got := fn(&ts)
		if got != tc.want {
			t.Errorf("timeAgo(-%v) = %q, want %q", tc.offset, got, tc.want)
		}
	}
}

// --- Landing page tests (unauthenticated) ---

func TestLandingPageWithOIDC(t *testing.T) {
	_, ts := newTestServer(t, oidcConfig())

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `href="/login"`) {
		t.Error("expected sign-in link to /login")
	}
	if !strings.Contains(body, "Sign in") {
		t.Error("expected 'Sign in' text")
	}
}

func TestLandingPageNoLeftNav(t *testing.T) {
	_, ts := newTestServer(t, oidcConfig())

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if strings.Contains(body, "left-nav") {
		t.Error("landing page should not have left navigation")
	}
}

func TestLandingPageShowsPublicApps(t *testing.T) {
	srv, ts := newTestServer(t, oidcConfig())

	app, _ := srv.DB.CreateApp("public-app", "owner")
	public := "public"
	srv.DB.UpdateApp(app.ID, db.AppUpdate{AccessType: &public})

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "public-app") {
		t.Error("expected public app on landing page")
	}
	if !strings.Contains(body, "Public apps") {
		t.Error("expected 'Public apps' section header")
	}
}

func TestLandingPageNoPublicAppsSection(t *testing.T) {
	_, ts := newTestServer(t, oidcConfig())

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if strings.Contains(body, "Public apps") {
		t.Error("should not show Public apps section when no public apps")
	}
}

func TestLandingPageHasHtmxScript(t *testing.T) {
	_, ts := newTestServer(t, oidcConfig())

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "htmx.min.js") {
		t.Error("expected htmx script tag")
	}
}

// --- Apps page tests (authenticated) ---

func TestAppsPageRendersWithAuth(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "test-user", auth.RoleAdmin)
	srv.DB.CreateApp("my-app", "test-user")

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "my-app") {
		t.Error("expected app in page")
	}
	if !strings.Contains(body, "Apps") {
		t.Error("expected 'Apps' in title")
	}
}

func TestAppsPageHasLeftNav(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "left-nav") {
		t.Error("apps page should have left navigation")
	}
	if !strings.Contains(body, `class="left-nav-link active"`) {
		t.Error("expected active nav link")
	}
}

func TestAppsPageNavActiveHighlighting(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	// The Apps link should be active
	if !strings.Contains(body, `href="/" class="left-nav-link active"`) {
		t.Error("expected Apps nav link to be active on / page")
	}
}

func TestAppsPageNavApiKeysHiddenWithoutOpenbao(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if strings.Contains(body, "API Keys") {
		t.Error("API Keys nav link should be hidden without openbao config")
	}
}

func TestAppsPageNavApiKeysShownWithOpenbao(t *testing.T) {
	_, ts := authServer(t, openbaoConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "API Keys") {
		t.Error("API Keys nav link should be visible with openbao config")
	}
}

func TestAppsPageEmptyStateByRole(t *testing.T) {
	tests := []struct {
		role     auth.Role
		roleName string
		expected string
	}{
		{auth.RolePublisher, "publisher", "New App"},
		{auth.RoleAdmin, "admin", "New App"},
		{auth.RoleViewer, "viewer", "No apps shared with you"},
	}

	for _, tt := range tests {
		t.Run(tt.roleName, func(t *testing.T) {
			_, ts := authServer(t, oidcConfig(), "user-1", tt.role)

			resp, err := http.Get(ts.URL + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body := readBody(t, resp)
			if !strings.Contains(body, tt.expected) {
				t.Errorf("expected %q in body for role %s", tt.expected, tt.roleName)
			}
		})
	}
}

func TestAppsPageSearchPreservesParams(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "user-1", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/?search=hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, `value="hello"`) {
		t.Error("expected search value preserved in form input")
	}
}

func TestAppsPageTagFilter(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)
	srv.DB.CreateTag("finance")

	resp, err := http.Get(ts.URL + "/?tag=finance")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "finance") {
		t.Error("expected tag name in chip bar")
	}
	// Active tag chip should have the "active" class.
	if !strings.Contains(body, "active") {
		t.Error("expected active class on selected tag chip")
	}
}

func TestAppsPageAppCardLinks(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("my-cool-app", "owner")

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, `href="/app/my-cool-app/"`) {
		t.Error("expected app card link to /app/my-cool-app/")
	}
}

func TestAppsPageAppRunningStatus(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("running-app", "owner")
	srv.Workers.Set("w1", server.ActiveWorker{AppID: app.ID})

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "running") {
		t.Error("expected 'running' status for app with active worker")
	}
}

func TestAppsPageAppStoppedStatus(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("stopped-app", "owner")

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "status-success") {
		t.Error("expected success status dot for enabled app with no workers")
	}
}

func TestAppsPageAppWithTitleAndDescription(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("titled-app", "owner")
	title := "My App Title"
	desc := "A great description"
	srv.DB.UpdateApp(app.ID, db.AppUpdate{Title: &title, Description: &desc})

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "My App Title") {
		t.Error("expected app title in rendered output")
	}
	if !strings.Contains(body, "A great description") {
		t.Error("expected app description in rendered output")
	}
}

func TestAppsPageGearIconForAdmin(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("managed-app", "owner")

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "app-card-gear") {
		t.Error("expected gear icon for admin user")
	}
	if !strings.Contains(body, "hx-get") {
		t.Error("expected hx-get attribute on gear icon")
	}
}

func TestAppsPageSidebarShell(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, `id="sidebar"`) {
		t.Error("expected sidebar container")
	}
	if !strings.Contains(body, "drawer-overlay") {
		t.Error("expected drawer overlay")
	}
	if !strings.Contains(body, "closeSidebar") {
		t.Error("expected closeSidebar function")
	}
}

func TestAppsPageSearchAndTagCombined(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)
	srv.DB.CreateTag("data")

	resp, err := http.Get(ts.URL + "/?search=foo&tag=data")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, `value="foo"`) {
		t.Error("expected search value preserved")
	}
	if !strings.Contains(body, "data") {
		t.Error("expected tag in dropdown")
	}
}

func TestAppsPageNoCaller(t *testing.T) {
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := oidcConfig()
	be := mock.New()
	srv := server.NewServer(cfg, be, database)

	uiHandler := New()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := auth.ContextWithUser(req.Context(), &auth.AuthenticatedUser{
				Sub: "no-caller-user",
			})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	uiHandler.RegisterRoutes(r, srv)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// --- DB error paths ---

func TestV0ModeDBError(t *testing.T) {
	srv, ts := newTestServer(t, defaultConfig())
	srv.DB.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
}

func TestLandingDBError(t *testing.T) {
	srv, ts := newTestServer(t, oidcConfig())
	srv.DB.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
}

func TestAppsPageDBError(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)
	srv.DB.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
}

// --- Deployment History page tests ---

func TestDeploymentsPageRequiresAuth(t *testing.T) {
	_, ts := newTestServer(t, oidcConfig())

	client := noRedirectClient()
	resp, err := client.Get(ts.URL + "/deployments")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "/login") {
		t.Errorf("expected redirect to /login, got %q", loc)
	}
	if !strings.Contains(loc, "return_url=%2Fdeployments") {
		t.Errorf("expected return_url=%%2Fdeployments in redirect, got %q", loc)
	}
}

func TestRequireAuthNonGetReturns401(t *testing.T) {
	_, ts := newTestServer(t, oidcConfig())

	form := url.Values{"name": {"test"}}
	resp, err := http.Post(ts.URL+"/ui/tokens", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// POST without auth should get 401, not a redirect.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestDeploymentsPageDBError(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)
	srv.DB.Close()

	resp, err := http.Get(ts.URL + "/deployments")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
}

func TestDeploymentsPageCustomPage(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "deployer", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("paged-app", "deployer")

	// Create 21 bundles to exceed deploymentsPerPage (20) and land on page 2.
	for i := range 21 {
		b, _ := srv.DB.CreateBundle("b-"+strings.Repeat("x", 10)+string(rune('a'+i)), app.ID, "deployer", false)
		srv.DB.ActivateBundle(app.ID, b.ID)
	}

	resp, err := http.Get(ts.URL + "/deployments?page=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Page 2") {
		t.Error("expected 'Page 2' in pagination info")
	}
}

func TestDeploymentsPageDeployedByFallback(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "admin-u", auth.RoleAdmin)

	app, _ := srv.DB.CreateApp("fb-app", "admin-u")
	// Deploy as "unknown-sub" which has no matching user row →
	// DeployedByName will be NULL, code should fall back to DeployedBy.
	b, _ := srv.DB.CreateBundle("bundle-fb-test", app.ID, "unknown-sub", false)
	srv.DB.ActivateBundle(app.ID, b.ID)

	resp, err := http.Get(ts.URL + "/deployments")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "unknown-sub") {
		t.Error("expected DeployedBy fallback value 'unknown-sub' in deployments table")
	}
}

func TestDeploymentsPageRendersEmpty(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/deployments")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Deployment History") {
		t.Error("expected page title")
	}
	if !strings.Contains(body, "No deployments found") {
		t.Error("expected empty state message")
	}
}

func TestDeploymentsPageNavActive(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/deployments")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, `href="/deployments" class="left-nav-link active"`) {
		t.Error("expected Deployment History nav link to be active")
	}
}

func TestDeploymentsPageWithData(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "deployer", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("deploy-app", "deployer")
	bundle, _ := srv.DB.CreateBundle("bundle-123456789", app.ID, "deployer", false)
	srv.DB.ActivateBundle(app.ID, bundle.ID)

	resp, err := http.Get(ts.URL + "/deployments")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "deploy-app") {
		t.Error("expected app name in deployments table")
	}
	if !strings.Contains(body, "bundle-1") {
		t.Error("expected truncated bundle ID in deployments table")
	}
}

func TestDeploymentsPageSearch(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/deployments?search=myapp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, `value="myapp"`) {
		t.Error("expected search value preserved in input")
	}
}

// --- API Keys page tests ---

func TestApiKeysPageRedirectsWithoutOpenbao(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	client := noRedirectClient()
	resp, err := client.Get(ts.URL + "/api-keys")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/" {
		t.Errorf("expected redirect to /, got %q", loc)
	}
}

func TestApiKeysPageRequiresAuth(t *testing.T) {
	_, ts := newTestServer(t, openbaoConfig())

	client := noRedirectClient()
	resp, err := client.Get(ts.URL + "/api-keys")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "/login") {
		t.Errorf("expected redirect to /login, got %q", loc)
	}
}

func TestApiKeysPageRendersWithOpenbao(t *testing.T) {
	_, ts := authServer(t, openbaoConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/api-keys")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "API Keys") {
		t.Error("expected page title")
	}
	if !strings.Contains(body, "OpenAI") {
		t.Error("expected service label")
	}
	if !strings.Contains(body, "not set") {
		t.Error("expected 'not set' status")
	}
}

func TestApiKeysPageNavActive(t *testing.T) {
	_, ts := authServer(t, openbaoConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/api-keys")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, `href="/api-keys" class="left-nav-link active"`) {
		t.Error("expected API Keys nav link to be active")
	}
}

func TestApiKeysPageHtmxForm(t *testing.T) {
	_, ts := authServer(t, openbaoConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/api-keys")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "hx-post") {
		t.Error("expected hx-post on credential form")
	}
}

// --- Profile page tests ---

func TestProfilePageRequiresAuth(t *testing.T) {
	_, ts := newTestServer(t, oidcConfig())

	client := noRedirectClient()
	resp, err := client.Get(ts.URL + "/profile")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "/login") {
		t.Errorf("expected redirect to /login, got %q", loc)
	}
	if !strings.Contains(loc, "return_url=%2Fprofile") {
		t.Errorf("expected return_url=%%2Fprofile in redirect, got %q", loc)
	}
}

func TestProfilePageRenders(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "jane", auth.RolePublisher)

	resp, err := http.Get(ts.URL + "/profile")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Profile") {
		t.Error("expected page title")
	}
	if !strings.Contains(body, "jane") {
		t.Error("expected user display name")
	}
	if !strings.Contains(body, "jane@test.com") {
		t.Error("expected user email")
	}
	if !strings.Contains(body, "publisher") {
		t.Error("expected user role")
	}
	if !strings.Contains(body, "Sign out") {
		t.Error("expected sign out button")
	}
}

func TestProfilePageNavActive(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/profile")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, `href="/profile" class="left-nav-link active"`) {
		t.Error("expected Profile nav link to be active")
	}
}

func TestProfilePageShowsTokenSection(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/profile")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, `id="tokens"`) {
		t.Error("expected tokens anchor")
	}
	if !strings.Contains(body, "Personal Access Tokens") {
		t.Error("expected PAT section header")
	}
	if !strings.Contains(body, "No tokens created") {
		t.Error("expected empty state when no tokens")
	}
}

func TestProfilePageShowsExistingTokens(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "pat-user", auth.RoleAdmin)

	// Create a token directly in DB.
	srv.DB.CreatePAT("tok-1", []byte("hash1"), "pat-user", "CI Token", nil)

	resp, err := http.Get(ts.URL + "/profile")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "CI Token") {
		t.Error("expected token name in table")
	}
	if !strings.Contains(body, "Revoke") {
		t.Error("expected revoke button")
	}
}

// --- PAT creation tests ---

func TestCreateTokenSuccess(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "token-user", auth.RoleAdmin)

	form := url.Values{"name": {"My Deploy Token"}}
	resp, err := http.Post(ts.URL+"/ui/tokens", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "by_") {
		t.Error("expected token value with by_ prefix")
	}
	if !strings.Contains(body, "Copy this token now") {
		t.Error("expected copy warning")
	}
}

func TestCreateTokenEmptyName(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "token-user", auth.RoleAdmin)

	form := url.Values{"name": {"  "}}
	resp, err := http.Post(ts.URL+"/ui/tokens", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "pat-error") {
		t.Error("expected error message for empty name")
	}
	if !strings.Contains(body, "required") {
		t.Error("expected 'required' in error message")
	}
}

func TestCreateTokenRequiresAuth(t *testing.T) {
	_, ts := newTestServer(t, oidcConfig())

	form := url.Values{"name": {"test"}}
	resp, err := http.Post(ts.URL+"/ui/tokens", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestCreateTokenForbiddenForPATSource(t *testing.T) {
	// Build a server where the caller has PAT source instead of session.
	cfg := oidcConfig()
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	database.UpsertUserWithRole("pat-caller", "pat@test.com", "pat-caller", "admin")

	be := mock.New()
	srv := server.NewServer(cfg, be, database)

	uiHandler := New()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := auth.ContextWithUser(req.Context(), &auth.AuthenticatedUser{Sub: "pat-caller"})
			ctx = auth.ContextWithCaller(ctx, &auth.CallerIdentity{
				Sub:    "pat-caller",
				Name:   "pat-caller",
				Role:   auth.RoleAdmin,
				Source: auth.AuthSourcePAT,
			})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	uiHandler.RegisterRoutes(r, srv)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	form := url.Values{"name": {"my-token"}}
	resp, err := http.Post(ts.URL+"/ui/tokens", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for PAT-sourced caller, got %d", resp.StatusCode)
	}
}

func TestCreateTokenDBError(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "db-err-user", auth.RoleAdmin)
	// Close DB so CreatePAT fails.
	srv.DB.Close()

	form := url.Values{"name": {"my-token"}}
	resp, err := http.Post(ts.URL+"/ui/tokens", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (htmx inline error), got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "pat-error") {
		t.Error("expected pat-error class in response")
	}
	if !strings.Contains(body, "Failed") {
		t.Error("expected failure message in response")
	}
}

func TestCreateTokenWithTTL(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "token-user", auth.RoleAdmin)

	form := url.Values{"name": {"CI Token"}, "expires_in": {"30d"}}
	resp, err := http.Post(ts.URL+"/ui/tokens", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "by_") {
		t.Error("expected token value with by_ prefix")
	}
	if !strings.Contains(body, "Expires in") {
		t.Error("expected expiry info in response")
	}
}

func TestCreateTokenNoExpiry(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "token-user", auth.RoleAdmin)

	form := url.Values{"name": {"Long-lived Token"}, "expires_in": {""}}
	resp, err := http.Post(ts.URL+"/ui/tokens", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "by_") {
		t.Error("expected token value with by_ prefix")
	}
	if !strings.Contains(body, "never expires") {
		t.Error("expected 'never expires' in response")
	}
}

func TestCreateTokenInvalidExpiry(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "token-user", auth.RoleAdmin)

	form := url.Values{"name": {"Bad Token"}, "expires_in": {"forever"}}
	resp, err := http.Post(ts.URL+"/ui/tokens", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "pat-error") {
		t.Error("expected error for invalid expiry")
	}
}

func TestProfilePageShowsTokenExpiry(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "expiry-user", auth.RoleAdmin)

	// Token with expiry.
	exp := time.Now().Add(90 * 24 * time.Hour).UTC().Format(time.RFC3339)
	srv.DB.CreatePAT("tok-exp", []byte("hash-exp"), "expiry-user", "Expiring Token", &exp)

	// Token without expiry.
	srv.DB.CreatePAT("tok-perm", []byte("hash-perm"), "expiry-user", "Permanent Token", nil)

	resp, err := http.Get(ts.URL + "/profile")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "Expiring Token") {
		t.Error("expected expiring token name")
	}
	if !strings.Contains(body, "Permanent Token") {
		t.Error("expected permanent token name")
	}
	// The Expires column header should be present.
	if !strings.Contains(body, "<th>Expires</th>") {
		t.Error("expected Expires column header")
	}
	// Permanent token should show "Never".
	if !strings.Contains(body, "Never") {
		t.Error("expected 'Never' for permanent token expiry")
	}
}

// --- Sidebar placeholder tests ---

func TestSidebarReturns404ForNonExistentApp(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/ui/apps/no-such-app/sidebar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent app, got %d", resp.StatusCode)
	}
}

func TestSidebarReturnsHTMLForOwner(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RolePublisher)
	app, err := srv.DB.CreateApp("my-app", "owner")
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/ui/apps/" + app.Name + "/sidebar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "sidebar-tabs") {
		t.Error("expected sidebar-tabs in response")
	}
	if !strings.Contains(body, "my-app") {
		t.Error("expected app name in sidebar")
	}
	// Owner should see Collaborators tab.
	if !strings.Contains(body, "Collaborators") {
		t.Error("expected Collaborators tab for owner")
	}
}

func TestSidebarReturnsHTMLForCollaborator(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "collab", auth.RoleViewer)
	app, err := srv.DB.CreateApp("collab-app", "someone-else")
	if err != nil {
		t.Fatal(err)
	}
	srv.DB.GrantAppAccess(app.ID, "collab", "user", "collaborator", "someone-else")

	resp, err := http.Get(ts.URL + "/ui/apps/" + app.Name + "/sidebar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "sidebar-tabs") {
		t.Error("expected sidebar-tabs")
	}
	// Collaborator should NOT see Collaborators tab (owner+ only).
	if strings.Contains(body, "Collaborators") {
		t.Error("collaborator should not see Collaborators tab")
	}
}

func TestSidebarReturns404ForViewer(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "viewer", auth.RoleViewer)
	app, err := srv.DB.CreateApp("viewer-app", "someone-else")
	if err != nil {
		t.Fatal(err)
	}
	srv.DB.GrantAppAccess(app.ID, "viewer", "user", "viewer", "someone-else")

	resp, err := http.Get(ts.URL + "/ui/apps/" + app.Name + "/sidebar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for viewer, got %d", resp.StatusCode)
	}
}

func TestSidebarReturns404ForUnauthenticated(t *testing.T) {
	srv, ts := newTestServer(t, oidcConfig())
	srv.DB.CreateApp("unauth-app", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/unauth-app/sidebar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unauthenticated, got %d", resp.StatusCode)
	}
}

func TestSidebarPreRendersOverview(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("overview-app", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/overview-app/sidebar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "overview-grid") {
		t.Error("expected pre-rendered overview content in sidebar")
	}
	if !strings.Contains(body, "tab-content") {
		t.Error("expected tab-content container in sidebar")
	}
}

// --- Tab endpoint tests ---

func TestOverviewTabReturnsHTML(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("ov-app", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/ov-app/tab/overview")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "overview-grid") {
		t.Error("expected overview-grid in response")
	}
	if !strings.Contains(body, "Status") {
		t.Error("expected Status card in overview")
	}
}

func TestSettingsTabReturnsHTML(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("set-app", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/set-app/tab/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `name="title"`) {
		t.Error("expected title field in settings")
	}
	if !strings.Contains(body, `name="description"`) {
		t.Error("expected description field in settings")
	}
	if !strings.Contains(body, `name="memory_limit"`) {
		t.Error("expected memory_limit field in settings")
	}
	if !strings.Contains(body, "Disable") || !strings.Contains(body, "Delete app") {
		t.Error("expected enable/disable and delete controls")
	}
}

func TestSettingsTabShowsEnableForDisabledApp(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("dis-app", "owner")
	srv.DB.SetAppEnabled(app.ID, false)

	resp, err := http.Get(ts.URL + "/ui/apps/dis-app/tab/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "Enable") {
		t.Error("expected Enable button for disabled app")
	}
	if !strings.Contains(body, "status-disabled") {
		t.Error("expected disabled status badge")
	}
}

func TestRuntimeTabReturnsHTML(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("rt-app", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/rt-app/tab/runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "runtime-view") {
		t.Error("expected runtime-view in response")
	}
	if !strings.Contains(body, "No active workers") {
		t.Error("expected empty state for runtime with no workers")
	}
}

func TestBundlesTabReturnsHTML(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("bun-app", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/bun-app/tab/bundles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "bundles-view") {
		t.Error("expected bundles-view in response")
	}
	if !strings.Contains(body, "No bundles") {
		t.Error("expected empty state for bundles")
	}
}

func TestBundlesTabShowsBundles(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("bun2-app", "owner")

	// Create a bundle.
	bundleID := "bun-" + app.ID[:8]
	srv.DB.CreateBundle(bundleID, app.ID, "owner", false)
	srv.DB.UpdateBundleStatus(bundleID, "ready")

	resp, err := http.Get(ts.URL + "/ui/apps/bun2-app/tab/bundles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "status-ready") {
		t.Error("expected ready status badge for bundle")
	}
}

func TestCollaboratorsTabReturns404ForNonOwner(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "collab", auth.RoleViewer)
	app, _ := srv.DB.CreateApp("acl-app", "someone-else")
	srv.DB.GrantAppAccess(app.ID, "collab", "user", "collaborator", "someone-else")

	resp, err := http.Get(ts.URL + "/ui/apps/" + app.Name + "/tab/collaborators")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for collaborator on collaborators tab, got %d", resp.StatusCode)
	}
}

func TestCollaboratorsTabReturnsHTMLForOwner(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RolePublisher)
	app, _ := srv.DB.CreateApp("acl2-app", "owner")
	srv.DB.GrantAppAccess(app.ID, "viewer1", "user", "viewer", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/" + app.Name + "/tab/collaborators")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "collaborators-view") {
		t.Error("expected collaborators-view in response")
	}
	if !strings.Contains(body, "access-type") {
		t.Error("expected access type selector")
	}
	if !strings.Contains(body, "viewer1") {
		t.Error("expected granted viewer in list")
	}
}

func TestCollaboratorsTabReturnsHTMLForAdmin(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "admin", auth.RoleAdmin)
	srv.DB.CreateApp("admin-acl-app", "someone-else")

	resp, err := http.Get(ts.URL + "/ui/apps/admin-acl-app/tab/collaborators")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for admin, got %d", resp.StatusCode)
	}
}

func TestLogsTabReturnsHTML(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("log-app", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/log-app/tab/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "log-viewer") {
		t.Error("expected log-viewer in response")
	}
}

func TestLogsWorkerTabReturns404ForMissingWorker(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("logw-app", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/logw-app/tab/logs/worker/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Should still return 200 with empty logs (worker just has no log data).
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "log-worker-view") {
		t.Error("expected log-worker-view in response")
	}
}

// --- All tab endpoints return 404 for unauthenticated requests ---

func TestAllTabEndpointsReturn404Unauthenticated(t *testing.T) {
	srv, _ := newTestServer(t, oidcConfig())
	srv.DB.CreateApp("unauth-tabs", "owner")

	// Create a separate unauthenticated test server.
	_, ts := newTestServer(t, oidcConfig())
	ts2srv, _ := newTestServer(t, oidcConfig())
	ts2srv.DB.CreateApp("unauth-tabs", "owner")

	paths := []string{
		"/ui/apps/unauth-tabs/sidebar",
		"/ui/apps/unauth-tabs/tab/overview",
		"/ui/apps/unauth-tabs/tab/settings",
		"/ui/apps/unauth-tabs/tab/runtime",
		"/ui/apps/unauth-tabs/tab/bundles",
		"/ui/apps/unauth-tabs/tab/collaborators",
		"/ui/apps/unauth-tabs/tab/logs",
		"/ui/apps/unauth-tabs/tab/logs/worker/w1",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("expected 404 for unauthenticated %s, got %d", path, resp.StatusCode)
			}
		})
	}
}

// --- Template function tests ---

func TestHumanBytes(t *testing.T) {
	fn := funcMap["humanBytes"].(func(uint64) string)

	cases := []struct {
		input uint64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{536870912, "512.0 MB"},
		{1073741824, "1.0 GB"},
	}

	for _, tc := range cases {
		got := fn(tc.input)
		if got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestDerefInt(t *testing.T) {
	fn := funcMap["derefInt"].(func(*int) string)

	if got := fn(nil); got != "" {
		t.Errorf("derefInt(nil) = %q, want empty", got)
	}
	v := 42
	if got := fn(&v); got != "42" {
		t.Errorf("derefInt(&42) = %q, want '42'", got)
	}
}

func TestDerefFloat(t *testing.T) {
	fn := funcMap["derefFloat"].(func(*float64) string)

	if got := fn(nil); got != "" {
		t.Errorf("derefFloat(nil) = %q, want empty", got)
	}
	v := 2.5
	if got := fn(&v); got != "2.5" {
		t.Errorf("derefFloat(&2.5) = %q, want '2.5'", got)
	}
}

func TestTimeAgoAcceptsBothStringTypes(t *testing.T) {
	fn := funcMap["timeAgo"].(func(any) string)

	// Test with string type (e.g. BundleRow.UploadedAt).
	ts := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	got := fn(ts)
	if got != "2 hours ago" {
		t.Errorf("timeAgo(string) = %q, want '2 hours ago'", got)
	}

	// Test with *string type (e.g. BundleRow.DeployedAt).
	got = fn(&ts)
	if got != "2 hours ago" {
		t.Errorf("timeAgo(*string) = %q, want '2 hours ago'", got)
	}

	// Test with nil.
	got = fn((*string)(nil))
	if got != "" {
		t.Errorf("timeAgo(nil) = %q, want empty", got)
	}

	// Test with empty string.
	got = fn("")
	if got != "" {
		t.Errorf(`timeAgo("") = %q, want empty`, got)
	}
}

// --- Version display ---

func TestVersionDisplayedInNav(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)
	srv.Version = "v1.2.3"

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "v1.2.3") {
		t.Error("expected version in left nav")
	}
}

// --- buildServiceEntries with real vault mock ---

func TestBuildServiceEntriesWithVaultMock(t *testing.T) {
	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "metadata") && strings.Contains(r.URL.Path, "openai") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data":{"versions":{}}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(vaultSrv.Close)

	cfg := oidcConfig()
	cfg.Openbao = &config.OpenbaoConfig{
		Address:    vaultSrv.URL,
		AdminToken: config.NewSecret("root"),
		Services: []config.ServiceConfig{
			{ID: "openai", Label: "OpenAI"},
			{ID: "github", Label: "GitHub"},
		},
	}

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	srv.VaultClient = integration.NewClient(vaultSrv.URL, func() string { return "root" })

	entries := buildServiceEntries(srv, "test-user")

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Status != "configured" {
		t.Errorf("openai status = %q, want 'configured'", entries[0].Status)
	}
	if entries[1].Status != "not_set" {
		t.Errorf("github status = %q, want 'not_set'", entries[1].Status)
	}
}

// ---------------------------------------------------------------------------
// Option A: Enrich tab tests with real data
// ---------------------------------------------------------------------------

// TestOverviewTabWithActiveData verifies the overview tab renders actual
// worker counts, session counts, view statistics, and bundle info.
func TestOverviewTabWithActiveData(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("ov-data-app", "owner")

	// Create and activate a bundle.
	bundleID := "bun-ov-" + app.ID[:8]
	srv.DB.CreateBundle(bundleID, app.ID, "owner", false)
	srv.DB.UpdateBundleStatus(bundleID, "ready")
	srv.DB.ActivateBundle(app.ID, bundleID)

	// Register two workers with sessions.
	srv.Workers.Set("w-ov-1", server.ActiveWorker{AppID: app.ID, BundleID: bundleID, StartedAt: time.Now()})
	srv.Workers.Set("w-ov-2", server.ActiveWorker{AppID: app.ID, BundleID: bundleID, StartedAt: time.Now()})
	srv.Sessions.Set("sess-1", session.Entry{WorkerID: "w-ov-1", UserSub: "owner", LastAccess: time.Now()})
	srv.Sessions.Set("sess-2", session.Entry{WorkerID: "w-ov-2", UserSub: "owner", LastAccess: time.Now()})
	srv.Sessions.Set("sess-3", session.Entry{WorkerID: "w-ov-2", UserSub: "other", LastAccess: time.Now()})

	// Seed DB sessions for view counts.
	srv.DB.CreateSession("db-s1", app.ID, "w-ov-1", "owner")
	srv.DB.CreateSession("db-s2", app.ID, "w-ov-2", "other")

	resp, err := http.Get(ts.URL + "/ui/apps/ov-data-app/tab/overview")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "2 active") {
		t.Error("expected '2 active' workers in overview")
	}
	if !strings.Contains(body, "3 sessions") {
		t.Error("expected '3 sessions' in overview")
	}
	if !strings.Contains(body, "2 views") {
		t.Error("expected '2 views' in overview")
	}
	if !strings.Contains(body, "status-success") {
		t.Error("expected success status dot for running app")
	}
	// Bundle should be shown.
	if !strings.Contains(body, "status-ready") {
		t.Error("expected ready status badge for active bundle")
	}
}

// TestSettingsTabWithTagsAndResourceLimits verifies the settings tab
// renders applied tags, available tags, and resource limit values.
func TestSettingsTabWithTagsAndResourceLimits(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("set-data-app", "owner")

	// Set resource limits.
	mem := "1g"
	cpu := 2.5
	maxW := 4
	title := "My Dashboard"
	desc := "A test app"
	srv.DB.UpdateApp(app.ID, db.AppUpdate{
		MemoryLimit:      &mem,
		CPULimit:         &cpu,
		MaxWorkersPerApp: &maxW,
		Title:            &title,
		Description:      &desc,
	})

	// Create tags: apply one, leave one available.
	tag1, _ := srv.DB.CreateTag("production")
	tag2, _ := srv.DB.CreateTag("staging")
	srv.DB.AddAppTag(app.ID, tag1.ID)

	resp, err := http.Get(ts.URL + "/ui/apps/set-data-app/tab/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	// Applied tag should appear as a chip.
	if !strings.Contains(body, "production") {
		t.Error("expected applied tag 'production' in settings")
	}
	// Available tag should appear in the add-tag select.
	if !strings.Contains(body, tag2.Name) {
		t.Error("expected available tag 'staging' in tag select")
	}
	// Resource limits.
	if !strings.Contains(body, `value="1g"`) {
		t.Error("expected memory_limit value '1g'")
	}
	if !strings.Contains(body, `value="2.5"`) {
		t.Error("expected cpu_limit value '2.5'")
	}
	if !strings.Contains(body, `value="4"`) {
		t.Error("expected max_workers value '4'")
	}
	// Title and description.
	if !strings.Contains(body, "My Dashboard") {
		t.Error("expected title 'My Dashboard'")
	}
	if !strings.Contains(body, "A test app") {
		t.Error("expected description 'A test app'")
	}
}

// TestSettingsTabRefreshScheduleShownForUnpinnedBundle verifies the
// refresh schedule field appears only when the active bundle is not pinned.
func TestSettingsTabRefreshScheduleShownForUnpinnedBundle(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("sched-app", "owner")

	// Unpinned bundle → refresh schedule should appear.
	bid := "bun-sched-" + app.ID[:8]
	srv.DB.CreateBundle(bid, app.ID, "owner", false)
	srv.DB.UpdateBundleStatus(bid, "ready")
	srv.DB.ActivateBundle(app.ID, bid)

	resp, err := http.Get(ts.URL + "/ui/apps/sched-app/tab/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, `name="refresh_schedule"`) {
		t.Error("expected refresh_schedule field for unpinned bundle")
	}
}

// TestSettingsTabRefreshScheduleHiddenForPinnedBundle verifies the
// refresh schedule field is absent when the active bundle is pinned.
func TestSettingsTabRefreshScheduleHiddenForPinnedBundle(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("pinned-app", "owner")

	// Pinned bundle → refresh schedule should be hidden.
	bid := "bun-pin-" + app.ID[:8]
	srv.DB.CreateBundle(bid, app.ID, "owner", true)
	srv.DB.UpdateBundleStatus(bid, "ready")
	srv.DB.ActivateBundle(app.ID, bid)

	resp, err := http.Get(ts.URL + "/ui/apps/pinned-app/tab/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if strings.Contains(body, `name="refresh_schedule"`) {
		t.Error("refresh_schedule field should be hidden for pinned bundle")
	}
}

// TestRuntimeTabWithWorkersAndSessions verifies the runtime tab renders
// worker table rows with status, sessions, and summary statistics.
func TestRuntimeTabWithWorkersAndSessions(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("rt-data-app", "owner")

	srv.Workers.Set("w-runtime-active-1", server.ActiveWorker{AppID: app.ID, StartedAt: time.Now()})
	srv.Workers.Set("w-runtime-draining", server.ActiveWorker{AppID: app.ID, Draining: true, StartedAt: time.Now()})

	srv.Sessions.Set("s-rt-1", session.Entry{WorkerID: "w-runtime-active-1", UserSub: "owner", LastAccess: time.Now()})
	srv.Sessions.Set("s-rt-2", session.Entry{WorkerID: "w-runtime-active-1", UserSub: "viewer", LastAccess: time.Now()})

	// Seed display name for user lookup.
	srv.DB.UpsertUserWithRole("owner", "owner@test.com", "Owner Name", "admin")

	// DB sessions for view counts.
	srv.DB.CreateSession("db-rt1", app.ID, "w-runtime-active-1", "owner")
	srv.DB.CreateSession("db-rt2", app.ID, "w-runtime-active-1", "viewer")
	srv.DB.CreateSession("db-rt3", app.ID, "w-runtime-draining", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/rt-data-app/tab/runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	// Should have worker rows, not the empty state.
	if strings.Contains(body, "No active workers") {
		t.Error("should not show empty state when workers exist")
	}
	// Worker IDs should be truncated in the table.
	if !strings.Contains(body, "w-runtim...") {
		t.Error("expected truncated worker ID 'w-runtim...' in table")
	}
	// Draining worker should show warning dot.
	if !strings.Contains(body, "status-warning") {
		t.Error("expected warning status dot for draining worker")
	}
	// Active worker should show success dot.
	if !strings.Contains(body, "status-success") {
		t.Error("expected success status dot for active worker")
	}
	// Session chip with display name.
	if !strings.Contains(body, "Owner Name") {
		t.Error("expected display name 'Owner Name' in session chip")
	}
	// Summary stats (now rendered as DaisyUI stat components).
	if !strings.Contains(body, "Sessions") {
		t.Error("expected 'Sessions' stat title in summary")
	}
	if !strings.Contains(body, "Total views") {
		t.Error("expected 'Total views' stat title in summary")
	}
}

// TestBundlesTabWithMultipleBundles verifies the bundles tab renders
// multiple bundles with the active badge and rollback buttons correctly.
func TestBundlesTabWithMultipleBundles(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("bun-data-app", "owner")

	// Create two bundles, activate the second.
	bid1 := "bun-old-" + app.ID[:8]
	bid2 := "bun-new-" + app.ID[:8]
	srv.DB.CreateBundle(bid1, app.ID, "owner", false)
	srv.DB.UpdateBundleStatus(bid1, "ready")
	srv.DB.CreateBundle(bid2, app.ID, "owner", false)
	srv.DB.UpdateBundleStatus(bid2, "ready")
	srv.DB.ActivateBundle(app.ID, bid2)

	resp, err := http.Get(ts.URL + "/ui/apps/bun-data-app/tab/bundles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	// Should not show empty state.
	if strings.Contains(body, "No bundles") {
		t.Error("should not show empty state with bundles present")
	}
	// Active bundle should have the "active" badge.
	if !strings.Contains(body, "badge-primary\">active") {
		t.Error("expected 'active' badge on the active bundle")
	}
	// Old bundle should have a Rollback button (it's ready and not active).
	if !strings.Contains(body, "Rollback") {
		t.Error("expected Rollback button for old ready bundle")
	}
	// Unpinned active bundle → show refresh button.
	if !strings.Contains(body, "Refresh dependencies") {
		t.Error("expected 'Refresh dependencies' button for unpinned bundle")
	}
}

// TestBundlesTabRefreshHiddenForPinnedBundle verifies the refresh button
// is hidden when the active bundle is pinned.
func TestBundlesTabRefreshHiddenForPinnedBundle(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("bun-pin-app", "owner")

	bid := "bun-pinned-" + app.ID[:8]
	srv.DB.CreateBundle(bid, app.ID, "owner", true)
	srv.DB.UpdateBundleStatus(bid, "ready")
	srv.DB.ActivateBundle(app.ID, bid)

	resp, err := http.Get(ts.URL + "/ui/apps/bun-pin-app/tab/bundles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if strings.Contains(body, "Refresh dependencies") {
		t.Error("should not show refresh button for pinned bundle")
	}
}

// TestBundlesTabNoRollbackForActiveBundle verifies the active bundle
// does not show a rollback button.
func TestBundlesTabNoRollbackForActiveBundle(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("bun-noroll-app", "owner")

	// Single bundle, active.
	bid := "bun-only-" + app.ID[:8]
	srv.DB.CreateBundle(bid, app.ID, "owner", false)
	srv.DB.UpdateBundleStatus(bid, "ready")
	srv.DB.ActivateBundle(app.ID, bid)

	resp, err := http.Get(ts.URL + "/ui/apps/bun-noroll-app/tab/bundles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if strings.Contains(body, "Rollback") {
		t.Error("active bundle should not show Rollback button")
	}
}

// TestCollaboratorsTabWithACLGrants verifies the collaborators tab
// renders grants when access type is ACL.
func TestCollaboratorsTabWithACLGrants(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RolePublisher)
	app, _ := srv.DB.CreateApp("acl-data-app", "owner")

	// Set ACL access type.
	acl := "acl"
	srv.DB.UpdateApp(app.ID, db.AppUpdate{AccessType: &acl})

	// Grant access and create user for display name.
	srv.DB.UpsertUserWithRole("alice", "alice@test.com", "Alice Smith", "viewer")
	srv.DB.GrantAppAccess(app.ID, "alice", "user", "viewer", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/" + app.Name + "/tab/collaborators")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	// ACL section should be visible.
	if !strings.Contains(body, "acl-section") {
		t.Error("expected acl-section for ACL access type")
	}
	// Grant should show display name.
	if !strings.Contains(body, "Alice Smith") {
		t.Error("expected display name 'Alice Smith' in grant list")
	}
	// Access type dropdown should be initialized with ACL.
	if !strings.Contains(body, `dropdown({value: 'acl'})`) {
		t.Error("expected dropdown initialized with 'acl' value")
	}
}

// TestCollaboratorsTabPublicAccessHidesACLSection verifies the ACL
// section is hidden when access type is not 'acl'.
func TestCollaboratorsTabPublicAccessHidesACLSection(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RolePublisher)
	app, _ := srv.DB.CreateApp("pub-acl-app", "owner")

	public := "public"
	srv.DB.UpdateApp(app.ID, db.AppUpdate{AccessType: &public})

	resp, err := http.Get(ts.URL + "/ui/apps/" + app.Name + "/tab/collaborators")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if strings.Contains(body, "acl-section") {
		t.Error("acl-section should be hidden for public access type")
	}
	if !strings.Contains(body, `dropdown({value: 'public'})`) {
		t.Error("expected dropdown initialized with 'public' value")
	}
}

// TestLogsTabWithLiveAndDeadWorkers verifies the logs tab shows both
// live workers from WorkerMap and dead workers from LogStore.
func TestLogsTabWithLiveAndDeadWorkers(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("log-data-app", "owner")

	// Live worker.
	srv.Workers.Set("w-log-live", server.ActiveWorker{AppID: app.ID, StartedAt: time.Now()})
	srv.Sessions.Set("s-log-1", session.Entry{WorkerID: "w-log-live", UserSub: "owner", LastAccess: time.Now()})

	// Dead worker in logstore.
	sender := srv.LogStore.Create("w-log-dead", app.ID)
	sender.Write("some log line")
	srv.LogStore.MarkEnded("w-log-dead")

	resp, err := http.Get(ts.URL + "/ui/apps/log-data-app/tab/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if strings.Contains(body, "No workers") {
		t.Error("should not show empty state when workers exist")
	}
	// Live worker.
	if !strings.Contains(body, "w-log-li...") {
		t.Error("expected truncated live worker ID")
	}
	if !strings.Contains(body, "status-active") {
		t.Error("expected active status for live worker")
	}
	// Dead worker.
	if !strings.Contains(body, "w-log-de...") {
		t.Error("expected truncated dead worker ID")
	}
	if !strings.Contains(body, "status-ended") {
		t.Error("expected ended status for dead worker")
	}
}

// TestLogsWorkerTabWithHistoricalLogs verifies historical logs are
// pre-rendered in the log output area.
func TestLogsWorkerTabWithHistoricalLogs(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("logw-data-app", "owner")

	// Create logstore entry with buffered lines.
	sender := srv.LogStore.Create("w-logw-hist", app.ID)
	sender.Write("[INFO] Server started on :3838")
	sender.Write("[INFO] Ready to accept connections")

	// Worker is still active.
	srv.Workers.Set("w-logw-hist", server.ActiveWorker{AppID: app.ID, StartedAt: time.Now()})

	resp, err := http.Get(ts.URL + "/ui/apps/logw-data-app/tab/logs/worker/w-logw-hist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "[INFO] Server started on :3838") {
		t.Error("expected first historical log line")
	}
	if !strings.Contains(body, "[INFO] Ready to accept connections") {
		t.Error("expected second historical log line")
	}
	// Active worker should have streaming button.
	if !strings.Contains(body, "Start streaming") {
		t.Error("expected 'Start streaming' button for active worker")
	}
}

// TestLogsWorkerTabInactiveWorkerNoStreamButton verifies the streaming
// button is hidden when the worker is not active.
func TestLogsWorkerTabInactiveWorkerNoStreamButton(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("logw-dead-app", "owner")

	// Dead worker with logs, not in WorkerMap.
	sender := srv.LogStore.Create("w-logw-dead", app.ID)
	sender.Write("[INFO] Shutting down")
	srv.LogStore.MarkEnded("w-logw-dead")

	resp, err := http.Get(ts.URL + "/ui/apps/logw-dead-app/tab/logs/worker/w-logw-dead")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "[INFO] Shutting down") {
		t.Error("expected historical log for dead worker")
	}
	if strings.Contains(body, `id="log-toggle"`) {
		t.Error("should not show streaming button for inactive worker")
	}
}

// ---------------------------------------------------------------------------
// Option B: computeAppStatus unit tests
// ---------------------------------------------------------------------------

func TestComputeAppStatusDisabled(t *testing.T) {
	srv, _ := authServer(t, oidcConfig(), "u", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("status-dis", "u")
	srv.DB.SetAppEnabled(app.ID, false)
	app.Enabled = false

	if got := computeAppStatus(srv, app); got != "disabled" {
		t.Errorf("computeAppStatus(disabled) = %q, want 'disabled'", got)
	}
}

func TestComputeAppStatusReady(t *testing.T) {
	srv, _ := authServer(t, oidcConfig(), "u", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("status-stop", "u")

	// Enabled, no workers → ready (idle, waiting for traffic).
	if got := computeAppStatus(srv, app); got != "idle" {
		t.Errorf("computeAppStatus(no workers) = %q, want 'idle'", got)
	}
}

func TestComputeAppStatusRunning(t *testing.T) {
	srv, _ := authServer(t, oidcConfig(), "u", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("status-run", "u")

	srv.Workers.Set("w-run-1", server.ActiveWorker{AppID: app.ID, StartedAt: time.Now()})

	if got := computeAppStatus(srv, app); got != "running" {
		t.Errorf("computeAppStatus(active worker) = %q, want 'running'", got)
	}
}

func TestComputeAppStatusStopping(t *testing.T) {
	srv, _ := authServer(t, oidcConfig(), "u", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("status-drain", "u")

	// All workers draining → stopping.
	srv.Workers.Set("w-drain-1", server.ActiveWorker{AppID: app.ID, Draining: true, StartedAt: time.Now()})
	srv.Workers.Set("w-drain-2", server.ActiveWorker{AppID: app.ID, Draining: true, StartedAt: time.Now()})

	if got := computeAppStatus(srv, app); got != "stopping" {
		t.Errorf("computeAppStatus(all draining) = %q, want 'stopping'", got)
	}
}

func TestComputeAppStatusRunningMixedDrain(t *testing.T) {
	srv, _ := authServer(t, oidcConfig(), "u", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("status-mix", "u")

	// One draining, one active → running.
	srv.Workers.Set("w-mix-1", server.ActiveWorker{AppID: app.ID, Draining: true, StartedAt: time.Now()})
	srv.Workers.Set("w-mix-2", server.ActiveWorker{AppID: app.ID, Draining: false, StartedAt: time.Now()})

	if got := computeAppStatus(srv, app); got != "running" {
		t.Errorf("computeAppStatus(mixed drain) = %q, want 'running'", got)
	}
}

// ---------------------------------------------------------------------------
// Option C: Granular per-tab RBAC boundary tests
// ---------------------------------------------------------------------------

// TestAdminCanAccessAllTabsOnOtherUsersApp verifies that an admin user
// can access every tab on an app they don't own.
func TestAdminCanAccessAllTabsOnOtherUsersApp(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "admin", auth.RoleAdmin)
	srv.DB.CreateApp("other-app", "someone-else")

	tabs := []string{"overview", "settings", "runtime", "bundles", "collaborators", "logs"}
	for _, tab := range tabs {
		t.Run(tab, func(t *testing.T) {
			resp, err := http.Get(ts.URL + "/ui/apps/other-app/tab/" + tab)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("admin should access %s tab, got %d", tab, resp.StatusCode)
			}
		})
	}
}

// TestCollaboratorCannotAccessCollaboratorsTabDirectly verifies that a
// collaborator gets 404 when hitting the collaborators tab endpoint.
func TestCollaboratorCannotAccessCollaboratorsTabDirectly(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "collab", auth.RoleViewer)
	app, _ := srv.DB.CreateApp("rbac-collab-app", "someone-else")
	srv.DB.GrantAppAccess(app.ID, "collab", "user", "collaborator", "someone-else")

	resp, err := http.Get(ts.URL + "/ui/apps/" + app.Name + "/tab/collaborators")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("collaborator should get 404 on collaborators tab, got %d", resp.StatusCode)
	}
}

// TestCollaboratorCanAccessNonOwnerTabs verifies that a collaborator can
// access all tabs except collaborators.
func TestCollaboratorCanAccessNonOwnerTabs(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "collab2", auth.RoleViewer)
	app, _ := srv.DB.CreateApp("rbac-tabs-app", "someone-else")
	srv.DB.GrantAppAccess(app.ID, "collab2", "user", "collaborator", "someone-else")

	tabs := []string{"overview", "settings", "runtime", "bundles", "logs"}
	for _, tab := range tabs {
		t.Run(tab, func(t *testing.T) {
			resp, err := http.Get(ts.URL + "/ui/apps/" + app.Name + "/tab/" + tab)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("collaborator should access %s tab, got %d", tab, resp.StatusCode)
			}
		})
	}
}

// TestViewerCannotAccessAnyTab verifies that a viewer (not collaborator)
// gets 404 on all tab endpoints.
func TestViewerCannotAccessAnyTab(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "viewer2", auth.RoleViewer)
	app, _ := srv.DB.CreateApp("rbac-viewer-app", "someone-else")
	srv.DB.GrantAppAccess(app.ID, "viewer2", "user", "viewer", "someone-else")

	tabs := []string{"overview", "settings", "runtime", "bundles", "collaborators", "logs"}
	for _, tab := range tabs {
		t.Run(tab, func(t *testing.T) {
			resp, err := http.Get(ts.URL + "/ui/apps/" + app.Name + "/tab/" + tab)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("viewer should get 404 on %s tab, got %d", tab, resp.StatusCode)
			}
		})
	}
}

// TestAdminCanAccessSidebarOnOtherUsersApp verifies the sidebar itself
// is accessible to admins on apps they don't own.
func TestAdminCanAccessSidebarOnOtherUsersApp(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "admin", auth.RoleAdmin)
	srv.DB.CreateApp("admin-side-app", "someone-else")

	resp, err := http.Get(ts.URL + "/ui/apps/admin-side-app/sidebar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin should access sidebar, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	// Admin should see the Collaborators tab in the sidebar.
	if !strings.Contains(body, "Collaborators") {
		t.Error("admin should see Collaborators tab on other user's app")
	}
}

// TestLogsWorkerTabRBACForCollaborator verifies a collaborator can
// access the logs worker sub-tab.
func TestLogsWorkerTabRBACForCollaborator(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "collab3", auth.RoleViewer)
	app, _ := srv.DB.CreateApp("rbac-logw-app", "someone-else")
	srv.DB.GrantAppAccess(app.ID, "collab3", "user", "collaborator", "someone-else")

	resp, err := http.Get(ts.URL + "/ui/apps/" + app.Name + "/tab/logs/worker/any-id")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("collaborator should access logs/worker tab, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Option D: Error and edge-case rendering tests
// ---------------------------------------------------------------------------

// TestOverviewTabNilActiveBundleShowsNone verifies the overview tab
// renders "None" when there is no active bundle.
func TestOverviewTabNilActiveBundleShowsNone(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("ov-nobundle", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/ov-nobundle/tab/overview")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "None") {
		t.Error("expected 'None' for nil active bundle in overview")
	}
}

// TestOverviewTabZeroWorkersAndSessions verifies the overview tab
// renders zero counts correctly.
func TestOverviewTabZeroWorkersAndSessions(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("ov-empty", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/ov-empty/tab/overview")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "0 active") {
		t.Error("expected '0 active' for zero workers")
	}
	if !strings.Contains(body, "0 sessions") {
		t.Error("expected '0 sessions' for zero sessions")
	}
	if !strings.Contains(body, "0 views") {
		t.Error("expected '0 views' for zero views")
	}
}

// TestSettingsTabNoTagsAvailable verifies settings renders correctly
// when no tags exist at all (no tag chips, but the combo form is visible).
func TestSettingsTabNoTagsAvailable(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("set-notags", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/set-notags/tab/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	// No individual tag chip spans should be present.
	if strings.Contains(body, "tag-remove") {
		t.Error("should not have tag remove buttons with no tags")
	}
	// Combo form is always visible (allows creating new tags).
	if !strings.Contains(body, "tag-add-form") {
		t.Error("tag-add-form should always be visible")
	}
}

// TestSettingsTabAllTagsApplied verifies the tag form is always visible
// (combo input supports both selecting and creating tags).
func TestSettingsTabAllTagsApplied(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("set-alltags", "owner")

	tag, _ := srv.DB.CreateTag("only-tag")
	srv.DB.AddAppTag(app.ID, tag.ID)

	resp, err := http.Get(ts.URL + "/ui/apps/set-alltags/tab/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	// Should have the tag chip.
	if !strings.Contains(body, "only-tag") {
		t.Error("expected applied tag 'only-tag'")
	}
	// Form is always visible (allows creating new tags).
	if !strings.Contains(body, "tag-add-form") {
		t.Error("tag-add-form should always be visible")
	}
}

// TestRuntimeTabEmptyWorkersShowsEmptyState verifies the empty state
// message appears when there are no workers.
func TestRuntimeTabEmptyWorkersShowsEmptyState(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("rt-empty", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/rt-empty/tab/runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "No active workers") {
		t.Error("expected 'No active workers' empty state")
	}
	// Summary should show zero counts (now rendered as stat components).
	if !strings.Contains(body, "Sessions") {
		t.Error("expected 'Sessions' stat in summary")
	}
}

// TestRuntimeTabAnonymousSessionDisplaysAnonymous verifies that a
// session without a user sub displays "anonymous".
func TestRuntimeTabAnonymousSessionDisplaysAnonymous(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("rt-anon", "owner")

	srv.Workers.Set("w-anon", server.ActiveWorker{AppID: app.ID, StartedAt: time.Now()})
	srv.Sessions.Set("s-anon", session.Entry{WorkerID: "w-anon", UserSub: "", LastAccess: time.Now()})

	resp, err := http.Get(ts.URL + "/ui/apps/rt-anon/tab/runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "anonymous") {
		t.Error("expected 'anonymous' display name for empty user sub")
	}
}

// TestBundlesTabEmptyShowsEmptyState verifies the bundles empty state.
func TestBundlesTabEmptyShowsEmptyState(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("bun-empty", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/bun-empty/tab/bundles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "No bundles") {
		t.Error("expected 'No bundles' empty state")
	}
	if strings.Contains(body, "Refresh dependencies") {
		t.Error("should not show refresh button with no bundles")
	}
}

// TestBundlesTabFailedBundleNoRollback verifies a failed bundle does
// not show a rollback button.
func TestBundlesTabFailedBundleNoRollback(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("bun-fail", "owner")

	bid := "bun-fail-" + app.ID[:8]
	srv.DB.CreateBundle(bid, app.ID, "owner", false)
	srv.DB.UpdateBundleStatus(bid, "failed")

	resp, err := http.Get(ts.URL + "/ui/apps/bun-fail/tab/bundles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if strings.Contains(body, "Rollback") {
		t.Error("failed bundle should not show Rollback button")
	}
	if !strings.Contains(body, "status-failed") {
		t.Error("expected failed status badge")
	}
}

// TestCollaboratorsTabEmptyGrantsShowsEmptyState verifies the empty
// state when access type is ACL but no grants exist.
func TestCollaboratorsTabEmptyGrantsShowsEmptyState(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RolePublisher)
	app, _ := srv.DB.CreateApp("acl-empty-app", "owner")

	acl := "acl"
	srv.DB.UpdateApp(app.ID, db.AppUpdate{AccessType: &acl})

	resp, err := http.Get(ts.URL + "/ui/apps/" + app.Name + "/tab/collaborators")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "No access grants") {
		t.Error("expected 'No access grants' empty state")
	}
}

// TestLogsTabNoWorkersShowsEmptyState verifies the logs tab empty state.
func TestLogsTabNoWorkersShowsEmptyState(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	srv.DB.CreateApp("log-empty", "owner")

	resp, err := http.Get(ts.URL + "/ui/apps/log-empty/tab/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "No workers") {
		t.Error("expected 'No workers' empty state")
	}
}

// TestSettingsTabStoppingStatusShowsDisabling verifies the settings tab
// shows "Disabling..." when the app status is "stopping".
func TestSettingsTabStoppingStatusShowsDisabling(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "owner", auth.RoleAdmin)
	app, _ := srv.DB.CreateApp("set-stopping", "owner")

	// All workers draining → status "stopping".
	srv.Workers.Set("w-stopping", server.ActiveWorker{AppID: app.ID, Draining: true, StartedAt: time.Now()})

	resp, err := http.Get(ts.URL + "/ui/apps/set-stopping/tab/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "Disabling...") {
		t.Error("expected 'Disabling...' for stopping status")
	}
	if !strings.Contains(body, "status-stopping") {
		t.Error("expected stopping status badge")
	}
}

// ---------------------------------------------------------------------------
// Admin page tests
// ---------------------------------------------------------------------------

func TestAdminPageRendersForAdmin(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)
	srv.DB.UpsertUserWithRole("user-1", "user1@example.com", "User One", "viewer")
	srv.DB.UpsertUserWithRole("user-2", "user2@example.com", "User Two", "publisher")

	resp, err := noRedirectClient().Get(ts.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "User One") {
		t.Error("expected User One in admin page")
	}
	if !strings.Contains(body, "User Two") {
		t.Error("expected User Two in admin page")
	}
	if !strings.Contains(body, "System checks") {
		t.Error("expected System checks section")
	}
	if !strings.Contains(body, ">Users<") {
		t.Error("expected Users section header")
	}
}

func TestAdminPageRedirectsForNonAdmin(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "pub", auth.RolePublisher)

	resp, err := noRedirectClient().Get(ts.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("expected 302, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/" {
		t.Errorf("expected redirect to /, got %q", got)
	}
}

func TestAdminPageNavLabelled(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, `href="/admin"`) {
		t.Error("expected /admin nav href")
	}
	if !strings.Contains(body, ">Admin<") {
		t.Error("expected Admin nav label")
	}
}

func TestAdminPageDisablesSelfControls(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	// Self-row should render role as a static badge, not a dropdown.
	if !strings.Contains(body, "(you)") {
		t.Error("expected '(you)' marker on caller's row")
	}
	// Make sure self-row has no PATCH dropdown targeting admin-1.
	if strings.Contains(body, `hx-patch="/api/v1/users/admin-1"`) {
		t.Error("self-row should not expose PATCH controls")
	}
}

func TestAdminUsersFragmentFiltersByRole(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)
	srv.DB.UpsertUserWithRole("v-1", "v1@example.com", "Viewer One", "viewer")
	srv.DB.UpsertUserWithRole("p-1", "p1@example.com", "Publisher One", "publisher")

	resp, err := http.Get(ts.URL + "/ui/admin/users?role=publisher")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Publisher One") {
		t.Error("expected Publisher One in filtered result")
	}
	if strings.Contains(body, "Viewer One") {
		t.Error("did not expect Viewer One when filtering by publisher")
	}
}

func TestAdminUsersFragmentFiltersByActive(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)
	srv.DB.UpsertUserWithRole("v-1", "v1@example.com", "Viewer One", "viewer")
	inactive := false
	srv.DB.UpdateUser("v-1", db.UserUpdate{Active: &inactive})
	srv.DB.UpsertUserWithRole("v-2", "v2@example.com", "Viewer Two", "viewer")

	resp, err := http.Get(ts.URL + "/ui/admin/users?active=inactive")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "Viewer One") {
		t.Error("expected Viewer One when filtering inactive")
	}
	if strings.Contains(body, "Viewer Two") {
		t.Error("did not expect Viewer Two when filtering inactive")
	}
}

func TestAdminUsersFragmentSearchesNameAndEmail(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)
	srv.DB.UpsertUserWithRole("alice-sub", "alice@example.com", "Alice", "viewer")
	srv.DB.UpsertUserWithRole("bob-sub", "bob@example.com", "Bob", "viewer")

	resp, err := http.Get(ts.URL + "/ui/admin/users?search=alic")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "Alice") {
		t.Error("expected Alice when searching 'alic'")
	}
	if strings.Contains(body, "Bob") {
		t.Error("did not expect Bob when searching 'alic'")
	}
}

func TestAdminUsersFragmentForbiddenForNonAdmin(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "viewer-1", auth.RoleViewer)

	resp, err := http.Get(ts.URL + "/ui/admin/users")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestAdminUsersFragmentSetsPushURL(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/ui/admin/users?search=foo&role=viewer")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	push := resp.Header.Get("HX-Push-Url")
	if !strings.HasPrefix(push, "/admin?") {
		t.Fatalf("expected HX-Push-Url starting with /admin?, got %q", push)
	}
	if !strings.Contains(push, "search=foo") || !strings.Contains(push, "role=viewer") {
		t.Errorf("expected filter params in push URL, got %q", push)
	}
}

func TestAdminUsersFragmentPaginates(t *testing.T) {
	srv, ts := authServer(t, oidcConfig(), "admin-1", auth.RoleAdmin)
	for i := 0; i < 25; i++ {
		srv.DB.UpsertUserWithRole(
			fmt.Sprintf("u-%02d", i),
			fmt.Sprintf("u%02d@example.com", i),
			fmt.Sprintf("User %02d", i),
			"viewer",
		)
	}

	resp, err := http.Get(ts.URL + "/ui/admin/users?page=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "Page 2 of 2") {
		t.Error("expected pagination label 'Page 2 of 2'")
	}
}
