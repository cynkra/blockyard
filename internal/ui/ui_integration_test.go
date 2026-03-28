//go:build ui_test

package ui

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/auth"
)

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
