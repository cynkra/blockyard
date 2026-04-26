package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCClient wraps the go-oidc provider and oauth2 config.
// Initialized once at server startup via Discover().
type OIDCClient struct {
	provider     *oidc.Provider
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	// httpClient carries a rewriting transport when issuer_discovery_url is set,
	// ensuring all server-side requests (token exchange, JWKS, refresh) use
	// the internal address.
	httpClient *http.Client
	// internalOrigin and publicOrigin are set when discoveryURL is provided,
	// enabling rewriting of discovery-document URLs from internal → public
	// for browser-facing redirects.
	internalOrigin string
	publicOrigin   string
}

// Discover performs OIDC discovery against the issuer URL and returns
// a configured client ready for the authorization code flow.
//
// If discoveryURL is non-empty, OIDC discovery is performed against that
// URL instead of issuerURL. This is useful in Docker environments where
// the IdP is reachable at a different internal address (e.g. http://dex:5556)
// than the public issuer URL used in tokens (e.g. http://localhost:5556).
// A custom HTTP transport rewrites all server-side requests to use the
// internal address. Token issuer validation still uses issuerURL.
func Discover(ctx context.Context, issuerURL, discoveryURL, clientID, clientSecret, redirectURL string) (*OIDCClient, error) {
	discoverFrom := issuerURL
	var httpClient *http.Client
	if discoveryURL != "" {
		ctx = oidc.InsecureIssuerURLContext(ctx, issuerURL)
		discoverFrom = discoveryURL

		// Install a rewriting HTTP client so that all server-side requests
		// (JWKS fetch, token exchange, refresh) reach the internal address.
		transport := &rewriteTransport{
			base:    http.DefaultTransport,
			oldBase: issuerURL,
			newBase: discoveryURL,
		}
		httpClient = &http.Client{Transport: transport}
		ctx = oidc.ClientContext(ctx, httpClient)
	}

	provider, err := oidc.NewProvider(ctx, discoverFrom)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}

	// When discovery was performed against an internal URL, the IdP may
	// return internal addresses in its endpoint URLs (e.g. Authentik uses
	// its own request origin). Rewrite them to the public issuer origin so
	// browser-facing redirects go to the right place. The rewriteTransport
	// handles the reverse mapping for server-side HTTP requests.
	var intOrigin, pubOrigin string
	endpoint := provider.Endpoint()
	if discoveryURL != "" {
		intOrigin = urlOrigin(discoveryURL)
		pubOrigin = urlOrigin(issuerURL)
		endpoint.AuthURL = rewriteURLOrigin(endpoint.AuthURL, intOrigin, pubOrigin)
		endpoint.TokenURL = rewriteURLOrigin(endpoint.TokenURL, intOrigin, pubOrigin)
	}

	oauth2Cfg := oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     endpoint,
		RedirectURL:  redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email", oidc.ScopeOfflineAccess},
	}

	verifier := provider.Verifier(&oidc.Config{
		ClientID: clientID,
	})

	return &OIDCClient{
		provider:       provider,
		oauth2Config:   oauth2Cfg,
		verifier:       verifier,
		httpClient:     httpClient,
		internalOrigin: intOrigin,
		publicOrigin:   pubOrigin,
	}, nil
}

// AuthCodeURL generates the authorization URL with a random state and nonce.
func (c *OIDCClient) AuthCodeURL(state string, nonce string) string {
	return c.oauth2Config.AuthCodeURL(state, oidc.Nonce(nonce))
}

// Exchange trades an authorization code for tokens.
func (c *OIDCClient) Exchange(ctx context.Context, code string) (*oauth2.Token, *oidc.IDToken, map[string]json.RawMessage, error) {
	if c.httpClient != nil {
		ctx = oidc.ClientContext(ctx, c.httpClient)
	}

	oauth2Token, err := c.oauth2Config.Exchange(ctx, code)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("token exchange: %w", err)
	}

	rawIDToken, ok := oauth2Token.Extra("id_token").(string)
	if !ok {
		return nil, nil, nil, fmt.Errorf("token response missing id_token")
	}

	idToken, err := c.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("id token verification: %w", err)
	}

	var allClaims map[string]json.RawMessage
	if err := idToken.Claims(&allClaims); err != nil {
		return nil, nil, nil, fmt.Errorf("extracting claims: %w", err)
	}

	return oauth2Token, idToken, allClaims, nil
}

// RefreshToken exchanges a refresh token for a new access token.
func (c *OIDCClient) RefreshToken(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
	if c.httpClient != nil {
		ctx = oidc.ClientContext(ctx, c.httpClient)
	}
	src := c.oauth2Config.TokenSource(ctx, &oauth2.Token{
		RefreshToken: refreshToken,
	})
	newToken, err := src.Token()
	if err != nil {
		return nil, fmt.Errorf("token refresh: %w", err)
	}
	return newToken, nil
}

// EndSessionEndpoint returns the IdP's end_session_endpoint if
// advertised in discovery metadata, or empty string otherwise.
func (c *OIDCClient) EndSessionEndpoint() string {
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := c.provider.Claims(&meta); err != nil {
		return ""
	}
	return rewriteURLOrigin(meta.EndSession, c.internalOrigin, c.publicOrigin)
}

// JWKSURI returns the IdP's jwks_uri from discovery metadata.
func (c *OIDCClient) JWKSURI() string {
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := c.provider.Claims(&meta); err != nil {
		return ""
	}
	return meta.JWKSURI
}

// rewriteTransport is an http.RoundTripper that rewrites request URLs,
// replacing oldBase with newBase. This allows the OIDC library to contact
// an internal address while the issuer URL stays external.
type rewriteTransport struct {
	base    http.RoundTripper
	oldBase string
	newBase string
}

// urlOrigin returns the scheme + authority of a URL (e.g.
// "https://auth.example.com" from "https://auth.example.com/path").
func urlOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Scheme + "://" + u.Host
}

// rewriteURLOrigin replaces the scheme+authority of u from oldOrigin to
// newOrigin. Returns u unchanged when oldOrigin is empty or doesn't match.
func rewriteURLOrigin(u, oldOrigin, newOrigin string) string {
	if oldOrigin != "" && strings.HasPrefix(u, oldOrigin) {
		return newOrigin + strings.TrimPrefix(u, oldOrigin)
	}
	return u
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL.String()
	if strings.HasPrefix(u, t.oldBase) {
		rewritten := t.newBase + strings.TrimPrefix(u, t.oldBase)
		newReq := req.Clone(req.Context())
		parsed, err := req.URL.Parse(rewritten)
		if err != nil {
			return nil, err
		}
		newReq.URL = parsed
		newReq.Host = parsed.Host
		req = newReq
	}
	return t.base.RoundTrip(req)
}
