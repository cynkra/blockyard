package ui

import (
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
	if !strings.Contains(body, "sidebar-header") {
		t.Error("expected sidebar-header in response")
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
	if !strings.Contains(body, "sidebar-header") {
		t.Error("expected sidebar-header")
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
