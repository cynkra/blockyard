package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// MockIdP is a minimal OIDC-compliant mock identity provider for
// integration tests. Serves:
//
//	GET  /.well-known/openid-configuration
//	GET  /jwks
//	POST /token
type MockIdP struct {
	Server     *httptest.Server
	signingKey *rsa.PrivateKey
	keyID      string

	// Sub and Groups to include in issued ID tokens.
	Sub    string
	Groups []string

	// Nonce to embed in the next ID token. Tests should set this to the
	// nonce extracted from the login redirect URL before calling /callback.
	Nonce string
}

// NewMockIdP starts a mock IdP on a random port. The default sub is
// "test-sub" with groups ["testers"].
func NewMockIdP() *MockIdP {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-key-1"

	m := &MockIdP{
		signingKey: key,
		keyID:      kid,
		Sub:        "test-sub",
		Groups:     []string{"testers"},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", m.handleDiscovery)
	mux.HandleFunc("GET /jwks", m.handleJWKS)
	mux.HandleFunc("POST /token", m.handleToken)

	m.Server = httptest.NewServer(mux)
	return m
}

func (m *MockIdP) IssuerURL() string { return m.Server.URL }

func (m *MockIdP) Close() { m.Server.Close() }

func (m *MockIdP) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	base := m.Server.URL
	doc := map[string]any{
		"issuer":                 base,
		"authorization_endpoint": base + "/authorize",
		"token_endpoint":         base + "/token",
		"jwks_uri":               base + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"subject_types_supported":               []string{"public"},
		"response_types_supported":              []string{"code"},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

func (m *MockIdP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	jwk := jose.JSONWebKey{
		Key:       &m.signingKey.PublicKey,
		KeyID:     m.keyID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(set)
}

// handleToken accepts any authorization code and returns a token
// response with a signed ID token.
func (m *MockIdP) handleToken(w http.ResponseWriter, r *http.Request) {
	// Read client_id from form (used as the audience).
	r.ParseForm()
	clientID := r.FormValue("client_id")
	if clientID == "" {
		// Fall back to basic auth.
		clientID, _, _ = r.BasicAuth()
	}

	idToken, err := m.issueIDToken(clientID, m.Nonce)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := map[string]any{
		"access_token":  "mock-access-token",
		"token_type":    "Bearer",
		"expires_in":    3600,
		"refresh_token": "mock-refresh-token",
		"id_token":      idToken,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// PrivateKey returns the RSA private key used for signing.
func (m *MockIdP) PrivateKey() *rsa.PrivateKey { return m.signingKey }

// Kid returns the key ID used in the JWKS.
func (m *MockIdP) Kid() string { return m.keyID }

// IssueJWT creates a JWT for control-plane Bearer auth (client credentials
// style). Same signing key as ID tokens, different claims structure.
func (m *MockIdP) IssueJWT(sub string, groups []string) string {
	now := time.Now()
	claims := map[string]any{
		"sub":    sub,
		"iss":    m.Server.URL,
		"aud":    "blockyard", // matches test OIDC config ClientID
		"exp":    now.Add(time.Hour).Unix(),
		"iat":    now.Unix(),
		"groups": groups,
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: m.signingKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", m.keyID),
	)
	if err != nil {
		panic("MockIdP.IssueJWT: " + err.Error())
	}

	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		panic("MockIdP.IssueJWT: " + err.Error())
	}
	return raw
}

// issueIDToken creates a signed JWT with the configured sub and groups.
func (m *MockIdP) issueIDToken(audience, nonce string) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: m.signingKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", m.keyID),
	)
	if err != nil {
		return "", err
	}

	now := time.Now()
	claims := map[string]any{
		"iss":    m.Server.URL,
		"sub":    m.Sub,
		"aud":    audience,
		"exp":    now.Add(1 * time.Hour).Unix(),
		"iat":    now.Unix(),
		"groups": m.Groups,
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}

	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", err
	}
	return raw, nil
}
