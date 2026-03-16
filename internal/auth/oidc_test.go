package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/cynkra/blockyard/internal/auth"
)

// TestDiscoverWithSplitURL verifies the split-URL OIDC mechanism:
// discovery is performed against an internal URL while issuer validation
// uses the public URL. This simulates a Docker environment where the IdP
// is reachable at a different address internally (e.g. http://dex:5556)
// than the public issuer URL used in tokens (e.g. http://localhost:5556).
func TestDiscoverWithSplitURL(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kid := "split-test-key"
	clientID := "split-test-client"

	// "Public" server — closed immediately so it's unreachable.
	// If any request leaks to this address without being rewritten,
	// the test fails with a connection error.
	publicServer := httptest.NewServer(http.NotFoundHandler())
	publicURL := publicServer.URL
	publicServer.Close()

	// "Internal" server — serves OIDC endpoints but reports publicURL
	// as the issuer, mirroring how Dex behaves inside Docker.
	mux := http.NewServeMux()

	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"issuer":                                publicURL,
			"authorization_endpoint":                publicURL + "/authorize",
			"token_endpoint":                        publicURL + "/token",
			"jwks_uri":                              publicURL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"subject_types_supported":               []string{"public"},
			"response_types_supported":              []string{"code"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(doc)
	})

	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		jwk := jose.JSONWebKey{
			Key:       &key.PublicKey,
			KeyID:     kid,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(set)
	})

	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		signer, err := jose.NewSigner(
			jose.SigningKey{Algorithm: jose.RS256, Key: key},
			(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		now := time.Now()
		claims := map[string]any{
			"iss": publicURL,
			"sub": "split-test-sub",
			"aud": clientID,
			"exp": now.Add(time.Hour).Unix(),
			"iat": now.Unix(),
		}

		raw, err := jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		resp := map[string]any{
			"access_token":  "split-access-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": "split-refresh-token",
			"id_token":      raw,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	internalServer := httptest.NewServer(mux)
	defer internalServer.Close()
	internalURL := internalServer.URL

	// Discover with split URLs: issuer=public, discovery=internal.
	oidcClient, err := auth.Discover(
		context.Background(),
		publicURL,
		internalURL,
		clientID,
		"split-test-secret",
		"http://localhost:8080/callback",
	)
	if err != nil {
		t.Fatalf("Discover with split URL: %v", err)
	}

	// AuthCodeURL should use the public URL (browser-facing).
	authURL := oidcClient.AuthCodeURL("test-state", "test-nonce")
	if !strings.HasPrefix(authURL, publicURL) {
		t.Errorf("AuthCodeURL should use public issuer URL;\n  got:  %s\n  want prefix: %s", authURL, publicURL)
	}

	// Token exchange must succeed via the internal URL.
	// The public server is closed — a connection error here proves
	// the rewriting transport failed to intercept the request.
	token, idToken, claims, err := oidcClient.Exchange(context.Background(), "test-code")
	if err != nil {
		t.Fatalf("Exchange via split URL: %v", err)
	}
	if token.AccessToken != "split-access-token" {
		t.Errorf("AccessToken = %q, want split-access-token", token.AccessToken)
	}
	if idToken.Subject != "split-test-sub" {
		t.Errorf("Subject = %q, want split-test-sub", idToken.Subject)
	}
	if claims == nil {
		t.Error("expected non-nil claims map")
	}

	// Token refresh must also succeed via the internal URL.
	refreshed, err := oidcClient.RefreshToken(context.Background(), "split-refresh-token")
	if err != nil {
		t.Fatalf("RefreshToken via split URL: %v", err)
	}
	if refreshed.AccessToken != "split-access-token" {
		t.Errorf("refreshed AccessToken = %q, want split-access-token", refreshed.AccessToken)
	}
}
