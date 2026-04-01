//go:build idp_test

package auth_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"net/netip"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/config"
)

const (
	keycloakImage    = "quay.io/keycloak/keycloak:26.0"
	keycloakClientID = "blockyard-client"
	keycloakSecret   = "test-client-secret"
	keycloakUser     = "testuser"
	keycloakPass     = "testpass"
)

// keycloakURL is set by TestMain after the Keycloak container starts.
var keycloakURL string

// containerID is set by TestMain for cleanup.
var containerID string

func TestMain(m *testing.M) {
	ctx := context.Background()

	cli, err := client.New(client.FromEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "docker client: %v\n", err)
		os.Exit(1)
	}
	defer cli.Close()

	// Pull image (may already be cached from CI pre-pull step).
	pullResp, err := cli.ImagePull(ctx, keycloakImage, client.ImagePullOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "image pull: %v\n", err)
		os.Exit(1)
	}
	io.Copy(io.Discard, pullResp)
	pullResp.Close()

	// Resolve the absolute path to the realm import file.
	realmFile, err := filepath.Abs("testdata/blockyard-test-realm.json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "abs path: %v\n", err)
		os.Exit(1)
	}

	// Create Keycloak container.
	kcPort := network.MustParsePort("8080/tcp")
	resp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: keycloakImage,
			Cmd:   []string{"start-dev", "--import-realm", "--health-enabled=true"},
			Env: []string{
				"KEYCLOAK_ADMIN=admin",
				"KEYCLOAK_ADMIN_PASSWORD=admin",
			},
			ExposedPorts: network.PortSet{kcPort: struct{}{}},
			Labels:       map[string]string{"blockyard-test": "idp"},
		},
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{
				kcPort: []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "0"}},
			},
			Binds: []string{
				realmFile + ":/opt/keycloak/data/import/blockyard-test-realm.json:ro",
			},
		},
		Name: "blockyard-idp-test",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "container create: %v\n", err)
		os.Exit(1)
	}
	containerID = resp.ID

	if _, err := cli.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		fmt.Fprintf(os.Stderr, "container start: %v\n", err)
		cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
		os.Exit(1)
	}

	// Get the mapped port.
	cResult, err := cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "container inspect: %v\n", err)
		cleanup(ctx, cli)
		os.Exit(1)
	}
	bindings := cResult.Container.NetworkSettings.Ports[kcPort]
	if len(bindings) == 0 {
		fmt.Fprintf(os.Stderr, "no port bindings for %s\n", kcPort)
		cleanup(ctx, cli)
		os.Exit(1)
	}
	hostPort := bindings[0].HostPort
	keycloakURL = fmt.Sprintf("http://127.0.0.1:%s", hostPort)

	// Wait for Keycloak to be ready (up to 120s).
	// Keycloak 26+ serves /health on the management port (9000), not the
	// application port (8080). Use the OIDC discovery endpoint as the
	// readiness signal instead.
	readyURL := keycloakURL + "/realms/master/.well-known/openid-configuration"
	deadline := time.Now().Add(120 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(readyURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	if !ready {
		fmt.Fprintf(os.Stderr, "keycloak did not become ready within 120s\n")
		// Dump container logs for debugging.
		if logReader, logErr := cli.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
			ShowStdout: true, ShowStderr: true,
		}); logErr == nil {
			fmt.Fprintf(os.Stderr, "--- keycloak container logs ---\n")
			io.Copy(os.Stderr, logReader)
			logReader.Close()
			fmt.Fprintf(os.Stderr, "--- end keycloak logs ---\n")
		}
		cleanup(ctx, cli)
		os.Exit(1)
	}

	code := m.Run()
	cleanup(ctx, cli)
	os.Exit(code)
}

func cleanup(ctx context.Context, cli *client.Client) {
	timeout := 10
	cli.ContainerStop(ctx, containerID, client.ContainerStopOptions{Timeout: &timeout})
	cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
}

// noRedirectClient returns an HTTP client that never follows redirects.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// setupIDPTestServer creates an httptest.Server wired to a real Keycloak IdP.
func setupIDPTestServer(t *testing.T) (*httptest.Server, *auth.Deps) {
	t.Helper()

	// Start the server first so we know its URL for the redirect URI.
	ts := httptest.NewUnstartedServer(nil)
	ts.Start()

	issuerURL := keycloakURL + "/realms/blockyard-test"
	redirectURL := ts.URL + "/callback"

	oidcClient, err := auth.Discover(
		context.Background(),
		issuerURL,
		"",
		keycloakClientID,
		keycloakSecret,
		redirectURL,
	)
	if err != nil {
		ts.Close()
		t.Fatalf("auth.Discover against Keycloak: %v", err)
	}

	secret := config.NewSecret("idp-test-session-secret")
	cfg := &config.Config{
		Server: config.ServerConfig{
			SessionSecret: &secret,
			ExternalURL:   ts.URL,
		},
		OIDC: &config.OidcConfig{
			IssuerURL:    issuerURL,
			ClientID:     keycloakClientID,
			ClientSecret: config.NewSecret(keycloakSecret),
			CookieMaxAge: config.Duration{Duration: 24 * time.Hour},
		},
	}

	deps := &auth.Deps{
		Config:       cfg,
		OIDCClient:   oidcClient,
		SigningKey:    auth.DeriveSigningKey(cfg.Server.SessionSecret.MustExpose()),
		UserSessions: auth.NewUserSessionStore(),
	}

	router := buildTestRouter(deps)
	ts.Config.Handler = router
	t.Cleanup(ts.Close)

	return ts, deps
}

// formActionRe extracts the action attribute from Keycloak's login form.
var formActionRe = regexp.MustCompile(`<form[^>]+id="kc-form-login"[^>]+action="([^"]+)"`)

// simulateLogin performs the full browser-simulated OIDC login flow:
//  1. GET /login on the test server
//  2. Follow redirect to Keycloak's authorize endpoint
//  3. Parse the login form and POST credentials
//  4. Follow the callback redirect back to the test server
//
// Returns the session cookie and the final response.
func simulateLogin(t *testing.T, serverURL string) (*http.Cookie, *http.Response) {
	t.Helper()
	httpClient := noRedirectClient()

	// 1. GET /login → 302 to Keycloak.
	loginURL := serverURL + "/login?return_url=/app/test/"
	resp, err := httpClient.Get(loginURL)
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /login: expected 302, got %d", resp.StatusCode)
	}

	stateCookie := findCookie(resp, "blockyard_oidc_state")
	if stateCookie == nil {
		t.Fatal("missing blockyard_oidc_state cookie after /login")
		return nil, nil
	}
	keycloakAuthURL := resp.Header.Get("Location")

	// 2. GET Keycloak authorize URL → login page HTML.
	req, _ := http.NewRequest("GET", keycloakAuthURL, nil)
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET keycloak auth: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Keycloak may return a 200 (login form) or a 302 (if already
	// authenticated). Handle the login form case.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET keycloak auth: expected 200, got %d", resp.StatusCode)
	}

	// Collect Keycloak session cookies.
	var kcCookies []*http.Cookie
	kcCookies = append(kcCookies, resp.Cookies()...)

	// Parse the form action URL.
	matches := formActionRe.FindSubmatch(body)
	if matches == nil {
		t.Fatalf("could not find login form action in Keycloak HTML:\n%s", string(body[:min(len(body), 2000)]))
		return nil, nil
	}
	// The action URL is HTML-encoded; decode &amp; to &.
	formAction := strings.ReplaceAll(string(matches[1]), "&amp;", "&")

	// 3. POST credentials to Keycloak.
	form := url.Values{
		"username": {keycloakUser},
		"password": {keycloakPass},
	}
	req, _ = http.NewRequest("POST", formAction, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range kcCookies {
		req.AddCookie(c)
	}
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST keycloak login: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("POST keycloak login: expected 302, got %d", resp.StatusCode)
	}

	// The redirect should point back to our test server's /callback.
	callbackURL := resp.Header.Get("Location")
	if !strings.Contains(callbackURL, "/callback") {
		t.Fatalf("expected redirect to /callback, got %s", callbackURL)
	}

	// 4. GET the callback URL on our test server with the state cookie.
	req, _ = http.NewRequest("GET", callbackURL, nil)
	req.AddCookie(stateCookie)
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET /callback: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /callback: expected 302, got %d", resp.StatusCode)
	}

	sessionCookie := findCookie(resp, "blockyard_session")
	if sessionCookie == nil {
		t.Fatal("missing blockyard_session cookie after /callback")
		return nil, resp
	}

	return sessionCookie, resp
}

func TestIDPDiscovery(t *testing.T) {
	issuerURL := keycloakURL + "/realms/blockyard-test"

	oidcClient, err := auth.Discover(
		context.Background(),
		issuerURL,
		"",
		keycloakClientID,
		keycloakSecret,
		"http://localhost/callback",
	)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if ep := oidcClient.EndSessionEndpoint(); ep == "" {
		t.Error("expected non-empty end_session_endpoint from Keycloak")
	}
}

func TestIDPFullAuthFlow(t *testing.T) {
	ts, _ := setupIDPTestServer(t)
	httpClient := noRedirectClient()

	sessionCookie, callbackResp := simulateLogin(t, ts.URL)

	// Verify callback redirected to the return_url.
	if loc := callbackResp.Header.Get("Location"); loc != "/app/test/" {
		t.Errorf("callback redirect = %q, want /app/test/", loc)
	}

	// Access a protected route with the session cookie.
	req, _ := http.NewRequest("GET", ts.URL+"/app/test/page", nil)
	req.AddCookie(sessionCookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET /app/test/page: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("authenticated request: expected 200, got %d", resp.StatusCode)
	}
}

func TestIDPLogout(t *testing.T) {
	ts, deps := setupIDPTestServer(t)
	httpClient := noRedirectClient()

	sessionCookie, _ := simulateLogin(t, ts.URL)

	// Decode the cookie to know the sub for session lookup.
	payload, err := auth.DecodeCookie(sessionCookie.Value, deps.SigningKey)
	if err != nil {
		t.Fatalf("decode cookie: %v", err)
	}

	// Verify session exists before logout.
	if deps.UserSessions.Get(payload.Sub) == nil {
		t.Fatal("expected session to exist before logout")
	}

	// POST /logout.
	req, _ := http.NewRequest("POST", ts.URL+"/logout", nil)
	req.AddCookie(sessionCookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST /logout: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("POST /logout: expected 302, got %d", resp.StatusCode)
	}

	// Verify redirect goes to Keycloak's end_session_endpoint.
	location := resp.Header.Get("Location")
	if !strings.Contains(location, keycloakURL) {
		t.Errorf("expected logout redirect to Keycloak, got %s", location)
	}

	// Verify session cookie is cleared.
	cleared := findCookie(resp, "blockyard_session")
	if cleared == nil || cleared.Value != "" {
		t.Error("expected session cookie to be cleared")
	}

	// Verify server-side session is removed.
	if deps.UserSessions.Get(payload.Sub) != nil {
		t.Error("expected server-side session to be removed after logout")
	}

	// Verify unauthenticated request has no identity in context.
	// AppAuthMiddleware is permissive (passes through without identity
	// when no session cookie is present), so the handler returns 200.
	req, _ = http.NewRequest("GET", ts.URL+"/app/test/page", nil)
	// Don't send the session cookie — it's been cleared.
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET after logout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 after logout (permissive middleware), got %d", resp.StatusCode)
	}
}

func TestIDPTokenRefresh(t *testing.T) {
	ts, deps := setupIDPTestServer(t)
	httpClient := noRedirectClient()

	sessionCookie, _ := simulateLogin(t, ts.URL)

	// Decode cookie to get sub.
	payload, err := auth.DecodeCookie(sessionCookie.Value, deps.SigningKey)
	if err != nil {
		t.Fatalf("decode cookie: %v", err)
	}

	// Get the original access token.
	session := deps.UserSessions.Get(payload.Sub)
	if session == nil {
		t.Fatal("expected session to exist")
		return
	}
	originalToken := session.AccessToken

	// Force the session to appear near-expiry so middleware triggers refresh.
	deps.UserSessions.UpdateTokens(
		payload.Sub,
		session.AccessToken,
		&session.RefreshToken,
		time.Now().Unix()-1, // expired
	)

	// Access a protected route — middleware should refresh the token.
	req, _ := http.NewRequest("GET", ts.URL+"/app/test/page", nil)
	req.AddCookie(sessionCookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET after token expiry: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after token refresh, got %d", resp.StatusCode)
	}

	// Verify the access token was actually refreshed.
	refreshedSession := deps.UserSessions.Get(payload.Sub)
	if refreshedSession == nil {
		t.Fatal("session missing after refresh")
		return
	}
	if refreshedSession.AccessToken == originalToken {
		t.Error("expected access token to change after refresh")
	}
	if refreshedSession.ExpiresAt <= time.Now().Unix() {
		t.Error("expected refreshed token expiry to be in the future")
	}
}

func TestIDPJWKSURI(t *testing.T) {
	_, deps := setupIDPTestServer(t)

	jwksURI := deps.OIDCClient.JWKSURI()
	if jwksURI == "" {
		t.Fatal("expected non-empty JWKS URI from Keycloak")
	}
	if !strings.Contains(jwksURI, "certs") && !strings.Contains(jwksURI, "jwks") {
		t.Errorf("expected JWKS URI to contain 'certs' or 'jwks', got %s", jwksURI)
	}
	if !strings.Contains(jwksURI, keycloakURL) {
		t.Errorf("expected JWKS URI to point to Keycloak (%s), got %s", keycloakURL, jwksURI)
	}
}

func TestIDPAuthCodeURL(t *testing.T) {
	_, deps := setupIDPTestServer(t)

	state := "test-state-value"
	nonce := "test-nonce-value"
	authURL := deps.OIDCClient.AuthCodeURL(state, nonce)

	if authURL == "" {
		t.Fatal("expected non-empty auth code URL")
	}

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("failed to parse auth code URL: %v", err)
	}

	query := parsed.Query()

	if got := query.Get("state"); got != state {
		t.Errorf("expected state=%q, got %q", state, got)
	}
	if got := query.Get("nonce"); got != nonce {
		t.Errorf("expected nonce=%q, got %q", nonce, got)
	}
	if got := query.Get("client_id"); got != keycloakClientID {
		t.Errorf("expected client_id=%q, got %q", keycloakClientID, got)
	}
	if got := query.Get("response_type"); got != "code" {
		t.Errorf("expected response_type=code, got %q", got)
	}
	if got := query.Get("redirect_uri"); got == "" {
		t.Error("expected non-empty redirect_uri in auth code URL")
	}

	// Verify the URL points to the Keycloak realm.
	if !strings.Contains(authURL, keycloakURL) {
		t.Errorf("expected auth URL to contain Keycloak URL %s, got %s", keycloakURL, authURL)
	}
}

func TestIDPEndSessionEndpoint(t *testing.T) {
	_, deps := setupIDPTestServer(t)

	endpoint := deps.OIDCClient.EndSessionEndpoint()
	if endpoint == "" {
		t.Fatal("expected non-empty end_session_endpoint from Keycloak")
	}

	// Verify it points to the correct Keycloak realm.
	if !strings.Contains(endpoint, keycloakURL) {
		t.Errorf("expected end_session_endpoint to contain Keycloak URL %s, got %s", keycloakURL, endpoint)
	}
	expectedRealmPath := "/realms/blockyard-test/"
	if !strings.Contains(endpoint, expectedRealmPath) {
		t.Errorf("expected end_session_endpoint to contain realm path %s, got %s", expectedRealmPath, endpoint)
	}

	// Verify it's a valid URL.
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("failed to parse end_session_endpoint: %v", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		t.Errorf("expected http(s) scheme, got %q", parsed.Scheme)
	}
}
