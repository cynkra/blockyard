package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCClient wraps the go-oidc provider and oauth2 config.
// Initialized once at server startup via Discover().
type OIDCClient struct {
	provider     *oidc.Provider
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	groupsClaim  string
}

// Discover performs OIDC discovery against the issuer URL and returns
// a configured client ready for the authorization code flow.
func Discover(ctx context.Context, issuerURL, clientID, clientSecret, redirectURL, groupsClaim string) (*OIDCClient, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}

	oauth2Cfg := oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile"},
	}

	verifier := provider.Verifier(&oidc.Config{
		ClientID: clientID,
	})

	return &OIDCClient{
		provider:     provider,
		oauth2Config: oauth2Cfg,
		verifier:     verifier,
		groupsClaim:  groupsClaim,
	}, nil
}

// AuthCodeURL generates the authorization URL with a random state and nonce.
func (c *OIDCClient) AuthCodeURL(state string, nonce string) string {
	return c.oauth2Config.AuthCodeURL(state, oidc.Nonce(nonce))
}

// Exchange trades an authorization code for tokens.
func (c *OIDCClient) Exchange(ctx context.Context, code string) (*oauth2.Token, *oidc.IDToken, map[string]json.RawMessage, error) {
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
	src := c.oauth2Config.TokenSource(ctx, &oauth2.Token{
		RefreshToken: refreshToken,
	})
	newToken, err := src.Token()
	if err != nil {
		return nil, fmt.Errorf("token refresh: %w", err)
	}
	return newToken, nil
}

// ExtractGroups reads the configured groups claim from the ID token's
// extra claims. Returns nil if the claim is missing or not an array.
func (c *OIDCClient) ExtractGroups(claims map[string]json.RawMessage) []string {
	raw, ok := claims[c.groupsClaim]
	if !ok {
		slog.Debug("groups claim not present in ID token", "claim", c.groupsClaim)
		return nil
	}

	var groups []string
	if err := json.Unmarshal(raw, &groups); err != nil {
		slog.Warn("groups claim is not a string array — ignoring",
			"claim", c.groupsClaim, "error", err)
		return nil
	}
	return groups
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
	return meta.EndSession
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
