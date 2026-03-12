package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/testutil"
)

// buildTestDeps creates an auth.Deps wired to a mock IdP.
func buildTestDeps(t *testing.T, idp *testutil.MockIdP) *auth.Deps {
	t.Helper()

	secret := config.NewSecret("test-session-secret")
	cfg := &config.Config{
		Server: config.ServerConfig{
			Token:         config.NewSecret("test-token"),
			SessionSecret: &secret,
			ExternalURL:   "http://localhost:8080",
		},
		OIDC: &config.OidcConfig{
			IssuerURL:    idp.IssuerURL(),
			ClientID:     "test-client",
			ClientSecret: config.NewSecret("test-client-secret"),
			GroupsClaim:  "groups",
			CookieMaxAge: config.Duration{Duration: 24 * 60 * 60 * 1e9}, // 24h
		},
	}

	oidcClient, err := auth.Discover(
		context.Background(),
		cfg.OIDC.IssuerURL,
		cfg.OIDC.ClientID,
		cfg.OIDC.ClientSecret.Expose(),
		cfg.Server.ExternalURL+"/callback",
		cfg.OIDC.GroupsClaim,
	)
	if err != nil {
		t.Fatal(err)
	}

	return &auth.Deps{
		Config:       cfg,
		OIDCClient:   oidcClient,
		SigningKey:    auth.DeriveSigningKey(cfg.Server.SessionSecret.Expose()),
		UserSessions: auth.NewUserSessionStore(),
	}
}

// buildTestRouter creates a chi router wired to auth handlers.
func buildTestRouter(deps *auth.Deps) http.Handler {
	r := chi.NewRouter()
	r.Get("/login", auth.LoginHandler(deps))
	r.Get("/callback", auth.CallbackHandler(deps))
	r.Post("/logout", auth.LogoutHandler(deps))
	r.Route("/app", func(sub chi.Router) {
		sub.Use(auth.AppAuthMiddleware(deps, nil))
		sub.Get("/{name}/*", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("app content"))
		})
	})
	return r
}

func findCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestLoginRedirectsToIdP(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)
	router := buildTestRouter(deps)

	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	location := w.Header().Get("Location")
	if !strings.HasPrefix(location, idp.IssuerURL()) {
		t.Errorf("expected redirect to IdP, got %s", location)
	}
}

func TestLoginSetsStateCookie(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)
	router := buildTestRouter(deps)

	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	stateCookie := findCookie(w.Result(), "blockyard_oidc_state")
	if stateCookie == nil {
		t.Fatal("expected blockyard_oidc_state cookie to be set")
	}
}

func TestLoginWithoutOIDCReturns404(t *testing.T) {
	deps := &auth.Deps{
		Config: &config.Config{},
	}
	router := buildTestRouter(deps)

	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestLoginOpenRedirectPrevention(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)
	router := buildTestRouter(deps)

	// These malicious return_urls should all result in a redirect to /
	// after the full login→callback flow completes.
	for _, malicious := range []string{"https://evil.com/", "//evil.com"} {
		t.Run(malicious, func(t *testing.T) {
			// 1. Login with malicious return_url.
			req := httptest.NewRequest("GET", "/login?return_url="+malicious, nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			stateCookie := findCookie(w.Result(), "blockyard_oidc_state")
			if stateCookie == nil {
				t.Fatal("missing state cookie")
			}
			csrfToken := extractStateParam(w.Header().Get("Location"))

			// 2. Callback with correct CSRF.
			req = httptest.NewRequest("GET", "/callback?code=test-code&state="+csrfToken, nil)
			req.AddCookie(stateCookie)
			w = httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusFound {
				t.Fatalf("expected 302, got %d", w.Code)
			}
			// Should redirect to / not to the malicious URL.
			if loc := w.Header().Get("Location"); loc != "/" {
				t.Errorf("expected redirect to /, got %q", loc)
			}
		})
	}
}

func TestCallbackCSRFMismatch(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)
	router := buildTestRouter(deps)

	// First do a login to get a state cookie.
	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	stateCookie := findCookie(w.Result(), "blockyard_oidc_state")
	if stateCookie == nil {
		t.Fatal("missing state cookie")
	}

	// Call callback with wrong state parameter.
	req = httptest.NewRequest("GET", "/callback?code=test-code&state=wrong-csrf", nil)
	req.AddCookie(stateCookie)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestFullAuthFlow(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)
	router := buildTestRouter(deps)

	// 1. GET /login → 302 to IdP
	req := httptest.NewRequest("GET", "/login?return_url=/app/test/", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("login: expected 302, got %d", w.Code)
	}

	stateCookie := findCookie(w.Result(), "blockyard_oidc_state")
	if stateCookie == nil {
		t.Fatal("missing state cookie after login")
	}

	// Extract CSRF token from the redirect URL's state parameter.
	location := w.Header().Get("Location")
	csrfToken := extractStateParam(location)

	// 2. GET /callback with code + correct state.
	req = httptest.NewRequest("GET", "/callback?code=test-code&state="+csrfToken, nil)
	req.AddCookie(stateCookie)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("callback: expected 302, got %d; body: %s", w.Code, w.Body.String())
	}

	// Verify redirect to return_url.
	if loc := w.Header().Get("Location"); loc != "/app/test/" {
		t.Errorf("callback redirect = %q, want /app/test/", loc)
	}

	// Verify session cookie was set.
	sessionCookie := findCookie(w.Result(), "blockyard_session")
	if sessionCookie == nil {
		t.Fatal("missing session cookie after callback")
	}

	// Verify state cookie was cleared (Max-Age=0 means delete).
	clearedState := findCookie(w.Result(), "blockyard_oidc_state")
	if clearedState != nil && clearedState.Value != "" {
		t.Error("expected state cookie to be cleared")
	}

	// 3. Verify server-side session exists.
	sess := deps.UserSessions.Get("test-sub")
	if sess == nil {
		t.Fatal("expected server-side session to exist")
	}
	if sess.AccessToken != "mock-access-token" {
		t.Errorf("AccessToken = %q", sess.AccessToken)
	}
	if len(sess.Groups) != 1 || sess.Groups[0] != "testers" {
		t.Errorf("Groups = %v", sess.Groups)
	}

	// 4. Access /app/test/ with session cookie — should succeed.
	req = httptest.NewRequest("GET", "/app/test/page", nil)
	req.AddCookie(sessionCookie)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("authenticated request: expected 200, got %d", w.Code)
	}
}

func TestUnauthenticatedProxyPassesThrough(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)
	router := buildTestRouter(deps)

	// Phase 1-2: middleware authenticates if possible but doesn't require it.
	// The proxy handler (not the middleware) decides whether to redirect.
	req := httptest.NewRequest("GET", "/app/my-app/page", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (pass-through), got %d", w.Code)
	}
}

func TestLogoutRemovesSession(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)
	router := buildTestRouter(deps)

	// Set up a session manually.
	deps.UserSessions.Set("test-sub", &auth.UserSession{
		AccessToken: "at-1",
	})
	cookie := &auth.CookiePayload{Sub: "test-sub", IssuedAt: nowUnix()}
	cookieValue, _ := cookie.Encode(deps.SigningKey)

	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(&http.Cookie{Name: "blockyard_session", Value: cookieValue})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Errorf("expected 302, got %d", w.Code)
	}

	// Verify session is removed.
	if deps.UserSessions.Get("test-sub") != nil {
		t.Error("expected session to be removed after logout")
	}

	// Verify cookie is cleared (value should be empty).
	cleared := findCookie(w.Result(), "blockyard_session")
	if cleared == nil || cleared.Value != "" {
		t.Error("expected session cookie to be cleared")
	}
}

func TestNoOIDCConfigPassesThrough(t *testing.T) {
	deps := &auth.Deps{
		Config: &config.Config{},
		// No OIDCClient, SigningKey, or UserSessions — v0 compat.
	}
	router := buildTestRouter(deps)

	req := httptest.NewRequest("GET", "/app/my-app/page", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should not redirect to login.
	if w.Code == http.StatusFound {
		loc := w.Header().Get("Location")
		if strings.HasPrefix(loc, "/login") {
			t.Error("v0 compat: request should not redirect to login without OIDC")
		}
	}
}

func TestMiddlewareExpiredCookieRedirects(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)
	router := buildTestRouter(deps)

	// Create a session and a cookie with an old issued_at.
	deps.UserSessions.Set("test-sub", &auth.UserSession{
		AccessToken: "at-1",
		ExpiresAt:   nowUnix() + 3600,
	})
	cookie := &auth.CookiePayload{Sub: "test-sub", IssuedAt: nowUnix() - 100000}
	cookieValue, _ := cookie.Encode(deps.SigningKey)

	req := httptest.NewRequest("GET", "/app/test/page", nil)
	req.AddCookie(&http.Cookie{Name: "blockyard_session", Value: cookieValue})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Phase 1-2: middleware passes through without identity when cookie is expired.
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for expired cookie (pass-through), got %d", w.Code)
	}
}

func TestMiddlewareMissingServerSession(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)
	router := buildTestRouter(deps)

	// Cookie is valid but no server-side session exists.
	cookie := &auth.CookiePayload{Sub: "no-such-user", IssuedAt: nowUnix()}
	cookieValue, _ := cookie.Encode(deps.SigningKey)

	req := httptest.NewRequest("GET", "/app/test/page", nil)
	req.AddCookie(&http.Cookie{Name: "blockyard_session", Value: cookieValue})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Phase 1-2: middleware passes through without identity when session is missing.
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for missing session (pass-through), got %d", w.Code)
	}
}

func TestMiddlewareContextCarriesUser(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)

	// Set up session.
	deps.UserSessions.Set("ctx-user", &auth.UserSession{
		Groups:      []string{"admin"},
		AccessToken: "at-ctx",
		ExpiresAt:   nowUnix() + 3600,
	})
	cookie := &auth.CookiePayload{Sub: "ctx-user", IssuedAt: nowUnix()}
	cookieValue, _ := cookie.Encode(deps.SigningKey)

	var captured *auth.AuthenticatedUser

	r := chi.NewRouter()
	r.Route("/app", func(sub chi.Router) {
		sub.Use(auth.AppAuthMiddleware(deps, nil))
		sub.Get("/{name}/*", func(w http.ResponseWriter, r *http.Request) {
			captured = auth.UserFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		})
	})

	req := httptest.NewRequest("GET", "/app/test/page", nil)
	req.AddCookie(&http.Cookie{Name: "blockyard_session", Value: cookieValue})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if captured == nil {
		t.Fatal("expected AuthenticatedUser in context")
	}
	if captured.Sub != "ctx-user" {
		t.Errorf("Sub = %q", captured.Sub)
	}
	if captured.AccessToken != "at-ctx" {
		t.Errorf("AccessToken = %q", captured.AccessToken)
	}
	if len(captured.Groups) != 1 || captured.Groups[0] != "admin" {
		t.Errorf("Groups = %v", captured.Groups)
	}
}

// --- helpers ---

func nowUnix() int64 {
	return auth.NowUnix()
}

func extractStateParam(url string) string {
	// Find state= in the URL.
	idx := strings.Index(url, "state=")
	if idx == -1 {
		return ""
	}
	rest := url[idx+6:]
	if end := strings.IndexByte(rest, '&'); end != -1 {
		rest = rest[:end]
	}
	return rest
}

func init() {
	// Suppress JSON output in tests.
	_ = json.Marshal
}
