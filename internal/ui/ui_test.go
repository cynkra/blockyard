package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

// newTestServer creates a minimal server and mounts UI routes for testing.
func newTestServer(t *testing.T, cfg *config.Config) (*server.Server, *httptest.Server) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	srv.RoleCache = auth.NewRoleMappingCache()

	r := chi.NewRouter()
	uiHandler := New()
	uiHandler.RegisterRoutes(r, srv)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return srv, ts
}

func defaultConfig() *config.Config {
	return &config.Config{
		Server:  config.ServerConfig{Token: config.NewSecret("test-token")},
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

// dashboardServer creates a test server with an injected user/caller for
// dashboard tests. Returns the server and test URL.
func dashboardServer(t *testing.T, cfg *config.Config, sub string, role auth.Role) (*server.Server, *httptest.Server) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	srv.RoleCache = auth.NewRoleMappingCache()

	uiHandler := New()
	r := chi.NewRouter()
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		ctx := auth.ContextWithUser(req.Context(), &auth.AuthenticatedUser{
			Sub:    sub,
			Groups: []string{"group"},
		})
		ctx = auth.ContextWithCaller(ctx, &auth.CallerIdentity{
			Sub:    sub,
			Groups: []string{"group"},
			Role:   role,
			Source: auth.AuthSourceSession,
		})
		uiHandler.root(srv)(w, req.WithContext(ctx))
	})

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return srv, ts
}

func TestNewDoesNotPanic(t *testing.T) {
	ui := New()
	if ui.pages == nil {
		t.Fatal("pages should not be nil")
	}
	if ui.static == nil {
		t.Fatal("static handler should not be nil")
	}
}

func TestEmbedContainsExpectedFiles(t *testing.T) {
	for _, path := range []string{
		"templates/base.html",
		"templates/landing.html",
		"templates/dashboard.html",
		"static/style.css",
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

// --- V0 mode tests (no OIDC) ---

func TestV0ModeRendersAllApps(t *testing.T) {
	srv, ts := newTestServer(t, defaultConfig())
	srv.DB.CreateApp("my-app", "owner-1")

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
		t.Error("expected app name in v0 page")
	}
}

func TestV0ModeReturns200WhenEmpty(t *testing.T) {
	_, ts := newTestServer(t, defaultConfig())

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "blockyard") {
		t.Error("expected blockyard in page")
	}
}

// --- Landing page tests (OIDC configured, unauthenticated) ---

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

func TestLandingPageShowsPublicApps(t *testing.T) {
	srv, ts := newTestServer(t, oidcConfig())

	// Create a public app — unauthenticated landing only shows public apps.
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

// --- Dashboard tests (authenticated) ---

func TestDashboardRendersWithAuth(t *testing.T) {
	srv, ts := dashboardServer(t, oidcConfig(), "test-user", auth.RoleAdmin)
	srv.DB.CreateApp("dashboard-app", "test-user")

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "test-user") {
		t.Error("expected user sub in dashboard")
	}
	if !strings.Contains(body, "Sign out") {
		t.Error("expected Sign out button")
	}
	if !strings.Contains(body, "dashboard-app") {
		t.Error("expected app in dashboard")
	}
	if !strings.Contains(body, "Dashboard") {
		t.Error("expected 'Dashboard' in title")
	}
}

func TestDashboardEmptyStateByRole(t *testing.T) {
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
			_, ts := dashboardServer(t, oidcConfig(), "user-1", tt.role)

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

func TestDashboardSearchPreservesParams(t *testing.T) {
	_, ts := dashboardServer(t, oidcConfig(), "user-1", auth.RoleAdmin)

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

func TestDashboardCredentialFlashSuccess(t *testing.T) {
	_, ts := dashboardServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/?credential_saved=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "Credential saved") {
		t.Error("expected success flash")
	}
}

func TestDashboardCredentialFlashError(t *testing.T) {
	_, ts := dashboardServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/?credential_error=bad+key")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "bad key") {
		t.Error("expected error flash message")
	}
}

func TestDashboardCredentialsSectionHiddenWithoutOpenbao(t *testing.T) {
	_, ts := dashboardServer(t, oidcConfig(), "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if strings.Contains(body, "Your API Keys") {
		t.Error("credentials section should be hidden without openbao config")
	}
}

func TestDashboardCredentialsSectionShown(t *testing.T) {
	cfg := oidcConfig()
	cfg.Openbao = &config.OpenbaoConfig{
		Address:    "http://localhost:8200",
		AdminToken: config.NewSecret("root"),
		Services: []config.ServiceConfig{
			{ID: "openai", Label: "OpenAI", Path: "apikeys/openai"},
		},
	}

	_, ts := dashboardServer(t, cfg, "u", auth.RoleAdmin)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "Your API Keys") {
		t.Error("expected credentials section when openbao services configured")
	}
	if !strings.Contains(body, "OpenAI") {
		t.Error("expected service label in credentials section")
	}
	if !strings.Contains(body, "not set") {
		t.Error("expected 'not set' status when VaultClient is nil")
	}
}

func TestDashboardTagFilter(t *testing.T) {
	srv, ts := dashboardServer(t, oidcConfig(), "u", auth.RoleAdmin)
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

func TestDashboardAppCardLinks(t *testing.T) {
	srv, ts := dashboardServer(t, oidcConfig(), "owner", auth.RoleAdmin)
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

func TestBaseTemplateDefaultTitle(t *testing.T) {
	ui := New()
	w := httptest.NewRecorder()
	// Execute the landing template with nil data — title should render.
	err := ui.pages["landing.html"].ExecuteTemplate(w, "base", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<title>") {
		t.Error("expected title tag in rendered HTML")
	}
}

// --- Helpers ---

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
