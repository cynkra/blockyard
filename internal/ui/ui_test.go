package ui

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/integration"
	"github.com/cynkra/blockyard/internal/server"
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

	r := chi.NewRouter()
	uiHandler := New()
	uiHandler.RegisterRoutes(r, srv)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
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
	fn := funcMap["timeAgo"].(func(*string) string)
	if got := fn(nil); got != "" {
		t.Errorf("timeAgo(nil) = %q, want empty", got)
	}
}

func TestTimeAgoParseError(t *testing.T) {
	fn := funcMap["timeAgo"].(func(*string) string)
	bad := "not-a-date"
	if got := fn(&bad); got != bad {
		t.Errorf("timeAgo(invalid) = %q, want %q (passthrough)", got, bad)
	}
}

func TestTimeAgoSingularForms(t *testing.T) {
	fn := funcMap["timeAgo"].(func(*string) string)

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
		{auth.RolePublisher, "publisher", "Deploy your first app"},
		{auth.RoleAdmin, "admin", "No apps deployed"},
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
		t.Error("expected tag name in dropdown")
	}
	if !strings.Contains(body, "selected") {
		t.Error("expected selected attribute on active tag")
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
	if !strings.Contains(body, "stopped") {
		t.Error("expected 'stopped' status for app with no workers")
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
	if !strings.Contains(body, "sidebar-overlay") {
		t.Error("expected sidebar overlay")
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
	if !strings.Contains(loc, "return_url=/deployments") {
		t.Errorf("expected return_url=/deployments in redirect, got %q", loc)
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
	if !strings.Contains(loc, "return_url=/profile") {
		t.Errorf("expected return_url=/profile in redirect, got %q", loc)
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

// --- Sidebar placeholder tests ---

func TestSidebarPlaceholderReturns200(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/ui/apps/test-app/sidebar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if body != "" {
		t.Error("expected empty response from sidebar placeholder")
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
// Multi-page navigation integration tests
// ---------------------------------------------------------------------------
//
// The tests above verify individual page behaviour in isolation. The section
// below tests cross-page properties of the navigation shell: consistency of
// the nav structure across pages, active-state exclusivity, page titles,
// layout skeleton, auth redirects with return URLs, version display, and a
// full sequential navigation flow.

// allAuthenticatedPaths returns the set of page paths used by cross-page
// tests. When openbao is enabled the list includes /api-keys.
func allAuthenticatedPaths(withOpenbao bool) []string {
	paths := []string{"/", "/deployments", "/profile"}
	if withOpenbao {
		paths = append(paths, "/api-keys")
	}
	return paths
}

// TestNavConsistencyAcrossPages verifies that every authenticated page
// renders the same left-nav structure with links to all pages.
func TestNavConsistencyAcrossPages(t *testing.T) {
	_, ts := authServer(t, openbaoConfig(), "nav-user", auth.RoleAdmin)

	expectedLinks := []string{
		`href="/"`,
		`href="/deployments"`,
		`href="/api-keys"`,
		`href="/profile"`,
	}

	for _, path := range allAuthenticatedPaths(true) {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body := readBody(t, resp)

			if !strings.Contains(body, `class="left-nav-brand"`) {
				t.Error("expected left-nav-brand")
			}
			for _, link := range expectedLinks {
				if !strings.Contains(body, link) {
					t.Errorf("expected %s on %s page", link, path)
				}
			}
		})
	}
}

// TestActiveNavExclusivity verifies that each page marks exactly one nav
// link as active and that it is the correct one.
func TestActiveNavExclusivity(t *testing.T) {
	_, ts := authServer(t, openbaoConfig(), "nav-user", auth.RoleAdmin)

	pages := []struct {
		path       string
		activeHref string
	}{
		{"/", "/"},
		{"/deployments", "/deployments"},
		{"/api-keys", "/api-keys"},
		{"/profile", "/profile"},
	}

	for _, pg := range pages {
		t.Run(pg.path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + pg.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body := readBody(t, resp)

			activeCount := strings.Count(body, `class="left-nav-link active"`)
			if activeCount != 1 {
				t.Errorf("expected exactly 1 active nav link, got %d", activeCount)
			}

			want := fmt.Sprintf(`href="%s" class="left-nav-link active"`, pg.activeHref)
			if !strings.Contains(body, want) {
				t.Errorf("expected %q to be active", pg.activeHref)
			}
		})
	}
}

// TestPageTitles verifies that each page sets the correct <title>.
func TestPageTitles(t *testing.T) {
	_, ts := authServer(t, openbaoConfig(), "title-user", auth.RoleAdmin)

	pages := []struct {
		path  string
		title string
	}{
		{"/", "Apps — blockyard"},
		{"/deployments", "Deployment History — blockyard"},
		{"/api-keys", "API Keys — blockyard"},
		{"/profile", "Profile — blockyard"},
	}

	for _, pg := range pages {
		t.Run(pg.path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + pg.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body := readBody(t, resp)
			want := "<title>" + pg.title + "</title>"
			if !strings.Contains(body, want) {
				t.Errorf("expected title %q", pg.title)
			}
		})
	}
}

// TestLandingPageTitle verifies the unauthenticated landing page title.
func TestLandingPageTitle(t *testing.T) {
	_, ts := newTestServer(t, oidcConfig())

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)
	if !strings.Contains(body, "<title>Sign in — blockyard</title>") {
		t.Error("expected title 'Sign in — blockyard'")
	}
}

// TestAllProtectedRoutesRedirect verifies every protected route redirects
// to /login with the correct return_url when unauthenticated.
func TestAllProtectedRoutesRedirect(t *testing.T) {
	_, ts := newTestServer(t, openbaoConfig())
	client := noRedirectClient()

	routes := []struct {
		path      string
		returnURL string
	}{
		{"/deployments", "return_url=/deployments"},
		{"/api-keys", "return_url=/api-keys"},
		{"/profile", "return_url=/profile"},
	}

	for _, rt := range routes {
		t.Run(rt.path, func(t *testing.T) {
			resp, err := client.Get(ts.URL + rt.path)
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
			if !strings.Contains(loc, rt.returnURL) {
				t.Errorf("expected %s in redirect, got %q", rt.returnURL, loc)
			}
		})
	}
}

// TestBaseHTMLStructureOnAllPages verifies the shared HTML skeleton
// (DOCTYPE, charset, stylesheet, htmx, layout classes) on every page.
func TestBaseHTMLStructureOnAllPages(t *testing.T) {
	_, ts := authServer(t, openbaoConfig(), "struct-user", auth.RoleAdmin)

	mustContain := []string{
		"<!DOCTYPE html>",
		`<html lang="en">`,
		`<meta charset="utf-8">`,
		`href="/static/style.css"`,
		`src="/static/htmx.min.js"`,
		`class="page-layout"`,
		`class="left-nav"`,
		`class="main-content"`,
	}

	for _, path := range allAuthenticatedPaths(true) {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body := readBody(t, resp)
			for _, s := range mustContain {
				if !strings.Contains(body, s) {
					t.Errorf("expected %q in body", s)
				}
			}
		})
	}
}

// TestLandingPageUsesSimpleLayout verifies the landing page uses the
// container-only layout without left-nav or page-layout.
func TestLandingPageUsesSimpleLayout(t *testing.T) {
	_, ts := newTestServer(t, oidcConfig())

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(t, resp)

	if strings.Contains(body, `class="page-layout"`) {
		t.Error("landing page should not have page-layout")
	}
	if strings.Contains(body, `class="left-nav"`) {
		t.Error("landing page should not have left-nav")
	}
	// Still has the base HTML skeleton
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Error("expected DOCTYPE")
	}
	if !strings.Contains(body, `src="/static/htmx.min.js"`) {
		t.Error("expected htmx script on landing")
	}
}

// TestAllPagesReturnHTML verifies every page returns text/html.
func TestAllPagesReturnHTML(t *testing.T) {
	_, ts := authServer(t, openbaoConfig(), "ct-user", auth.RoleAdmin)

	for _, path := range allAuthenticatedPaths(true) {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			ct := resp.Header.Get("Content-Type")
			if !strings.Contains(ct, "text/html") {
				t.Errorf("expected text/html, got %q", ct)
			}
		})
	}
}

// TestVersionDisplayedOnAllPages verifies the version string appears in
// the nav on every authenticated page, not just the apps page.
func TestVersionDisplayedOnAllPages(t *testing.T) {
	srv, ts := authServer(t, openbaoConfig(), "ver-user", auth.RoleAdmin)
	srv.Version = "v2.0.0-test"

	for _, path := range allAuthenticatedPaths(true) {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body := readBody(t, resp)
			if !strings.Contains(body, "v2.0.0-test") {
				t.Errorf("expected version on %s page", path)
			}
		})
	}
}

// TestApiKeysNavHiddenOnAllPagesWithoutOpenbao verifies the API Keys nav
// link is absent on every page when openbao is not configured.
func TestApiKeysNavHiddenOnAllPagesWithoutOpenbao(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	for _, path := range allAuthenticatedPaths(false) {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body := readBody(t, resp)
			if strings.Contains(body, `href="/api-keys"`) {
				t.Error("API Keys link should be hidden without openbao")
			}
		})
	}
}

// TestApiKeysNavVisibleOnAllPagesWithOpenbao verifies the API Keys nav
// link appears on every page when openbao is configured.
func TestApiKeysNavVisibleOnAllPagesWithOpenbao(t *testing.T) {
	_, ts := authServer(t, openbaoConfig(), "u", auth.RoleAdmin)

	for _, path := range allAuthenticatedPaths(true) {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body := readBody(t, resp)
			if !strings.Contains(body, `href="/api-keys"`) {
				t.Error("API Keys link should be visible with openbao")
			}
		})
	}
}

// TestNavigationIntactWithQueryParams verifies that search/filter/page
// query parameters do not break the navigation shell.
func TestNavigationIntactWithQueryParams(t *testing.T) {
	_, ts := authServer(t, oidcConfig(), "u", auth.RoleAdmin)

	urls := []struct {
		url        string
		activeHref string
	}{
		{"/?search=foo&tag=bar", "/"},
		{"/deployments?search=myapp&page=1", "/deployments"},
	}

	for _, u := range urls {
		t.Run(u.url, func(t *testing.T) {
			resp, err := http.Get(ts.URL + u.url)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", resp.StatusCode)
			}

			body := readBody(t, resp)
			if !strings.Contains(body, `class="left-nav"`) {
				t.Error("expected left-nav")
			}

			want := fmt.Sprintf(`href="%s" class="left-nav-link active"`, u.activeHref)
			if !strings.Contains(body, want) {
				t.Errorf("expected %q to be active", u.activeHref)
			}
		})
	}
}

// TestRoleBasedNavigationStructure verifies that all roles see the same
// navigation links (the nav itself does not vary by role).
func TestRoleBasedNavigationStructure(t *testing.T) {
	roles := []auth.Role{auth.RoleAdmin, auth.RolePublisher, auth.RoleViewer}

	for _, role := range roles {
		t.Run(role.String(), func(t *testing.T) {
			_, ts := authServer(t, oidcConfig(), "role-user", role)

			resp, err := http.Get(ts.URL + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body := readBody(t, resp)
			for _, link := range []string{`href="/"`, `href="/deployments"`, `href="/profile"`} {
				if !strings.Contains(body, link) {
					t.Errorf("expected %s for role %s", link, role)
				}
			}
		})
	}
}

// TestFullNavigationFlow simulates a user navigating through all pages
// in sequence and verifies that navigation state updates correctly at
// each step.
func TestFullNavigationFlow(t *testing.T) {
	srv, ts := authServer(t, openbaoConfig(), "flow-user", auth.RoleAdmin)

	// Seed data so pages have meaningful content.
	app, _ := srv.DB.CreateApp("nav-test-app", "flow-user")
	b, _ := srv.DB.CreateBundle("b-nav-test-id", app.ID, "flow-user", false)
	srv.DB.ActivateBundle(app.ID, b.ID)
	srv.Version = "v3.0.0-flow"

	steps := []struct {
		path        string
		activeHref  string
		mustContain []string
	}{
		{
			path:        "/",
			activeHref:  "/",
			mustContain: []string{"nav-test-app", "Apps"},
		},
		{
			path:        "/deployments",
			activeHref:  "/deployments",
			mustContain: []string{"nav-test-app", "Deployment History"},
		},
		{
			path:        "/api-keys",
			activeHref:  "/api-keys",
			mustContain: []string{"API Keys", "OpenAI"},
		},
		{
			path:        "/profile",
			activeHref:  "/profile",
			mustContain: []string{"Profile", "flow-user"},
		},
		{
			// Navigate back to apps.
			path:        "/",
			activeHref:  "/",
			mustContain: []string{"nav-test-app"},
		},
	}

	for _, step := range steps {
		resp, err := http.Get(ts.URL + step.path)
		if err != nil {
			t.Fatalf("%s: %v", step.path, err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("%s: expected 200, got %d", step.path, resp.StatusCode)
		}

		body := readBody(t, resp)
		resp.Body.Close()

		// Exactly one active link.
		if n := strings.Count(body, `class="left-nav-link active"`); n != 1 {
			t.Errorf("%s: expected 1 active nav link, got %d", step.path, n)
		}

		// Correct link is active.
		want := fmt.Sprintf(`href="%s" class="left-nav-link active"`, step.activeHref)
		if !strings.Contains(body, want) {
			t.Errorf("%s: expected %q to be active", step.path, step.activeHref)
		}

		// Page-specific content present.
		for _, s := range step.mustContain {
			if !strings.Contains(body, s) {
				t.Errorf("%s: expected %q in body", step.path, s)
			}
		}

		// Version always visible.
		if !strings.Contains(body, "v3.0.0-flow") {
			t.Errorf("%s: expected version in nav", step.path)
		}
	}
}
