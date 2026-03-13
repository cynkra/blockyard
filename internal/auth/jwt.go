package auth

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWKSCache caches the IdP's JSON Web Key Set for JWT validation on the
// control plane.
type JWKSCache struct {
	mu          sync.RWMutex
	keys        map[string]*rsa.PublicKey // kid -> parsed public key
	jwksURI     string
	httpClient  *http.Client
	lastRefresh time.Time
}

// refreshCooldown is the minimum time between JWKS refreshes.
const refreshCooldown = 60 * time.Second

// NewJWKSCache fetches the JWKS from the IdP's jwks_uri and initializes
// the cache. Called once at startup, alongside OIDC discovery.
func NewJWKSCache(jwksURI string) (*JWKSCache, error) {
	c := &JWKSCache{
		keys:       make(map[string]*rsa.PublicKey),
		jwksURI:    jwksURI,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
	if err := c.fetchKeys(); err != nil {
		return nil, fmt.Errorf("initial JWKS fetch: %w", err)
	}
	c.lastRefresh = time.Now()
	return c, nil
}

// Refresh re-fetches the JWKS from the IdP. No-op if called within the
// cooldown period. Returns true if the keys were actually refreshed.
func (c *JWKSCache) Refresh() (bool, error) {
	c.mu.RLock()
	elapsed := time.Since(c.lastRefresh)
	c.mu.RUnlock()

	if elapsed < refreshCooldown {
		return false, nil
	}

	if err := c.fetchKeys(); err != nil {
		return false, err
	}

	c.mu.Lock()
	c.lastRefresh = time.Now()
	c.mu.Unlock()
	return true, nil
}

// ErrKidNotFound is returned when a JWT's kid does not match any key in the JWKS.
var ErrKidNotFound = errors.New("kid not found in JWKS")

// Validate parses and validates a JWT, returning its claims.
// On kid-not-found, refreshes the JWKS once and retries.
func (c *JWKSCache) Validate(tokenStr, issuer, audience string) (*JWTClaims, error) {
	claims, err := c.tryValidate(tokenStr, issuer, audience)
	if err != nil {
		if errors.Is(err, ErrKidNotFound) {
			if _, refreshErr := c.Refresh(); refreshErr != nil {
				return nil, refreshErr
			}
			return c.tryValidate(tokenStr, issuer, audience)
		}
		return nil, err
	}
	return claims, nil
}

func (c *JWKSCache) tryValidate(tokenStr, issuer, audience string) (*JWTClaims, error) {
	claims := &JWTClaims{}

	_, err := jwt.ParseWithClaims(tokenStr, claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, fmt.Errorf("missing kid in token header")
		}

		c.mu.RLock()
		key, found := c.keys[kid]
		c.mu.RUnlock()

		if !found {
			return nil, ErrKidNotFound
		}
		return key, nil
	},
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512"}),
	)
	if err != nil {
		return nil, err
	}

	return claims, nil
}

// jwkJSON represents a single key in a JWKS response.
type jwkJSON struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (c *JWKSCache) fetchKeys() error {
	resp, err := c.httpClient.Get(c.jwksURI)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS endpoint returned %d", resp.StatusCode)
	}

	var jwks struct {
		Keys []jwkJSON `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("decode JWKS: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kid == "" || k.Kty != "RSA" {
			continue
		}
		pub, err := parseRSAPublicKeyFromJWK(k)
		if err != nil {
			slog.Debug("skipping unparseable JWK", "kid", k.Kid, "error", err)
			continue
		}
		keys[k.Kid] = pub
	}

	c.mu.Lock()
	c.keys = keys
	c.mu.Unlock()
	return nil
}

// parseRSAPublicKeyFromJWK converts a JWK JSON object to an *rsa.PublicKey
// by decoding the base64url-encoded n (modulus) and e (exponent) fields.
func parseRSAPublicKeyFromJWK(k jwkJSON) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() {
		return nil, fmt.Errorf("exponent too large")
	}

	return &rsa.PublicKey{
		N: n,
		E: int(e.Int64()),
	}, nil
}

// JWTClaims holds the claims extracted from a validated JWT.
type JWTClaims struct {
	jwt.RegisteredClaims
	Groups []string `json:"groups,omitempty"`
	// Extra holds additional claims for non-standard group claim names.
	Extra map[string]any `json:"-"`
}

// ExtractGroups extracts groups from the configured claim name.
// Checks the typed Groups field first, then falls back to the extra
// claims map (for non-standard claim names like "cognito:groups").
func (c *JWTClaims) ExtractGroups(groupsClaim string) []string {
	// If the configured claim is "groups" and the typed field has values, use it
	if groupsClaim == "groups" && len(c.Groups) > 0 {
		return c.Groups
	}

	// Otherwise, check the extra claims map
	raw, ok := c.Extra[groupsClaim]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	groups := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			groups = append(groups, s)
		}
	}
	return groups
}

// UnmarshalJSON implements custom unmarshaling to capture extra claims.
func (c *JWTClaims) UnmarshalJSON(data []byte) error {
	// First unmarshal the known fields
	type Alias JWTClaims
	aux := &struct {
		*Alias
	}{Alias: (*Alias)(c)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}

	// Then unmarshal everything into the extra map
	if err := json.Unmarshal(data, &c.Extra); err != nil {
		return err
	}
	// Remove known fields from extra
	for _, key := range []string{"sub", "iss", "aud", "exp", "iat", "nbf", "jti", "groups"} {
		delete(c.Extra, key)
	}
	return nil
}
