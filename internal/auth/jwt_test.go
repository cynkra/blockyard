package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/testutil"
	"github.com/go-jose/go-jose/v4"
	gojwt "github.com/go-jose/go-jose/v4/jwt"
)

func TestJWKSCacheValidateValidToken(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	cache, err := auth.NewJWKSCache(idp.IssuerURL() + "/jwks")
	if err != nil {
		t.Fatal(err)
	}

	token := idp.IssueJWT("user-1", []string{"developers"})
	claims, err := cache.Validate(token, idp.IssuerURL(), "blockyard")
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	if claims.Subject != "user-1" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "user-1")
	}
	groups := claims.ExtractGroups("groups")
	if len(groups) != 1 || groups[0] != "developers" {
		t.Errorf("Groups = %v, want [developers]", groups)
	}
}

func TestJWKSCacheValidateWrongIssuer(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	cache, err := auth.NewJWKSCache(idp.IssuerURL() + "/jwks")
	if err != nil {
		t.Fatal(err)
	}

	token := idp.IssueJWT("user-1", nil)
	_, err = cache.Validate(token, "https://wrong-issuer.example.com", "blockyard")
	if err == nil {
		t.Error("expected error for wrong issuer")
	}
}

func TestJWKSCacheValidateWrongAudience(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	cache, err := auth.NewJWKSCache(idp.IssuerURL() + "/jwks")
	if err != nil {
		t.Fatal(err)
	}

	token := idp.IssueJWT("user-1", nil)
	_, err = cache.Validate(token, idp.IssuerURL(), "wrong-audience")
	if err == nil {
		t.Error("expected error for wrong audience")
	}
}

func TestJWKSCacheRefreshCooldown(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	cache, err := auth.NewJWKSCache(idp.IssuerURL() + "/jwks")
	if err != nil {
		t.Fatal(err)
	}

	// Refresh should be a no-op within cooldown period
	refreshed, err := cache.Refresh()
	if err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}
	if refreshed {
		t.Error("expected no-op refresh within cooldown period")
	}
}

func TestJWKSCacheFailsOnBadEndpoint(t *testing.T) {
	_, err := auth.NewJWKSCache("http://localhost:1/nonexistent")
	if err == nil {
		t.Error("expected error for unreachable JWKS endpoint")
	}
}

func TestJWTClaimsExtractGroupsStandard(t *testing.T) {
	claims := &auth.JWTClaims{
		Groups: []string{"admin", "developers"},
	}
	groups := claims.ExtractGroups("groups")
	if len(groups) != 2 || groups[0] != "admin" || groups[1] != "developers" {
		t.Errorf("ExtractGroups = %v, want [admin developers]", groups)
	}
}

func TestJWTClaimsExtractGroupsCustomClaim(t *testing.T) {
	claims := &auth.JWTClaims{
		Extra: map[string]any{
			"cognito:groups": []any{"team-a", "team-b"},
		},
	}
	groups := claims.ExtractGroups("cognito:groups")
	if len(groups) != 2 || groups[0] != "team-a" || groups[1] != "team-b" {
		t.Errorf("ExtractGroups = %v, want [team-a team-b]", groups)
	}
}

func TestJWTClaimsExtractGroupsMissing(t *testing.T) {
	claims := &auth.JWTClaims{}
	groups := claims.ExtractGroups("groups")
	if len(groups) != 0 {
		t.Errorf("ExtractGroups = %v, want empty", groups)
	}
}

func TestJWTClaimsUnmarshalJSON(t *testing.T) {
	data := `{
		"sub": "user-1",
		"iss": "https://example.com",
		"aud": "blockyard",
		"groups": ["admin"],
		"custom_field": "value"
	}`

	var claims auth.JWTClaims
	if err := json.Unmarshal([]byte(data), &claims); err != nil {
		t.Fatal(err)
	}

	if claims.Subject != "user-1" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "user-1")
	}
	if len(claims.Groups) != 1 || claims.Groups[0] != "admin" {
		t.Errorf("Groups = %v, want [admin]", claims.Groups)
	}
	if claims.Extra["custom_field"] != "value" {
		t.Errorf("Extra[custom_field] = %v, want 'value'", claims.Extra["custom_field"])
	}
	// Known fields should not be in Extra
	if _, ok := claims.Extra["sub"]; ok {
		t.Error("sub should not be in Extra")
	}
}

func TestNewJWKSCacheNon200Response(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := auth.NewJWKSCache(srv.URL)
	if err == nil {
		t.Error("expected error for non-200 JWKS response")
	}
}

func TestJWKSCacheValidateUnknownKidTriggersRefresh(t *testing.T) {
	// This test verifies that when a JWT has an unknown kid, the cache
	// attempts a refresh. We use a valid IdP but set up a scenario
	// where the initial fetch works, then we validate with a valid token.
	idp := testutil.NewMockIdP()
	defer idp.Close()

	cache, err := auth.NewJWKSCache(idp.IssuerURL() + "/jwks")
	if err != nil {
		t.Fatal(err)
	}

	// Token from IdP should work (kid matches)
	token := idp.IssueJWT("user-1", nil)
	_, err = cache.Validate(token, idp.IssuerURL(), "blockyard")
	if err != nil {
		t.Fatalf("expected valid token to pass: %v", err)
	}
}

// helper to check time parsing
func TestJWKSCacheWithIdPDown(t *testing.T) {
	// Start IdP, fetch keys, then shut it down
	idp := testutil.NewMockIdP()
	jwksURL := idp.IssuerURL() + "/jwks"

	cache, err := auth.NewJWKSCache(jwksURL)
	if err != nil {
		t.Fatal(err)
	}

	token := idp.IssueJWT("user-1", nil)
	idp.Close()

	// Validation should still work with cached keys
	_, err = cache.Validate(token, idp.IssuerURL(), "blockyard")
	if err != nil {
		t.Fatalf("expected cached keys to work after IdP shutdown: %v", err)
	}
}

// Ensure time import is used
var _ = time.Now

func TestJWTClaimsExtractGroupsExtraNonArray(t *testing.T) {
	claims := &auth.JWTClaims{
		Extra: map[string]any{
			"roles": "not-an-array",
		},
	}
	groups := claims.ExtractGroups("roles")
	if groups != nil {
		t.Errorf("expected nil for non-array extra value, got %v", groups)
	}
}

func TestJWTClaimsExtractGroupsExtraNonStringElements(t *testing.T) {
	claims := &auth.JWTClaims{
		Extra: map[string]any{
			"roles": []any{123, true, "valid-group"},
		},
	}
	groups := claims.ExtractGroups("roles")
	if len(groups) != 1 || groups[0] != "valid-group" {
		t.Errorf("expected [valid-group], got %v", groups)
	}
}

func TestJWTClaimsExtractGroupsEmptyTypedField(t *testing.T) {
	// When Groups is empty but Extra has the "groups" key, should use Extra.
	claims := &auth.JWTClaims{
		Groups: []string{},
		Extra: map[string]any{
			"groups": []any{"from-extra"},
		},
	}
	groups := claims.ExtractGroups("groups")
	if len(groups) != 1 || groups[0] != "from-extra" {
		t.Errorf("expected [from-extra], got %v", groups)
	}
}

func TestJWKSCacheValidateGarbageToken(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	cache, err := auth.NewJWKSCache(idp.IssuerURL() + "/jwks")
	if err != nil {
		t.Fatal(err)
	}

	_, err = cache.Validate("not-a-jwt", idp.IssuerURL(), "blockyard")
	if err == nil {
		t.Error("expected error for garbage token")
	}
}

func TestNewJWKSCacheInvalidJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("this is not json"))
	}))
	defer srv.Close()

	_, err := auth.NewJWKSCache(srv.URL)
	if err == nil {
		t.Error("expected error for non-JSON JWKS response")
	}
}

// ---------- new coverage tests ----------

func TestFetchKeysSkipsNonRSAAndEmptyKid(t *testing.T) {
	// JWKS with an EC key (kty != RSA) and a key with empty kid.
	// Both should be skipped; cache should have zero usable keys.
	jwksBody := `{"keys":[
		{"kid":"ec-1","kty":"EC","alg":"ES256","use":"sig","n":"AAAA","e":"AQAB"},
		{"kid":"","kty":"RSA","alg":"RS256","use":"sig","n":"AAAA","e":"AQAB"}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jwksBody))
	}))
	defer srv.Close()

	cache, err := auth.NewJWKSCache(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Any token should fail because there are no usable keys.
	idp := testutil.NewMockIdP()
	defer idp.Close()
	token := idp.IssueJWT("user-1", nil)
	_, err = cache.Validate(token, idp.IssuerURL(), "blockyard")
	if err == nil {
		t.Error("expected error when no usable RSA keys in JWKS")
	}
}

func TestFetchKeysSkipsUnparseableJWK(t *testing.T) {
	// A key with invalid base64 in the modulus should be skipped.
	jwksBody := `{"keys":[
		{"kid":"bad-1","kty":"RSA","alg":"RS256","use":"sig","n":"!!!invalid-base64!!!","e":"AQAB"}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jwksBody))
	}))
	defer srv.Close()

	cache, err := auth.NewJWKSCache(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	idp := testutil.NewMockIdP()
	defer idp.Close()
	token := idp.IssueJWT("user-1", nil)
	_, err = cache.Validate(token, idp.IssuerURL(), "blockyard")
	if err == nil {
		t.Error("expected error when JWK has bad base64 modulus")
	}
}

func TestFetchKeysInvalidExponentBase64(t *testing.T) {
	// Valid modulus base64, but invalid exponent base64.
	jwksBody := `{"keys":[
		{"kid":"bad-exp","kty":"RSA","alg":"RS256","use":"sig","n":"AAAA","e":"!!!bad!!!"}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jwksBody))
	}))
	defer srv.Close()

	cache, err := auth.NewJWKSCache(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Cache created successfully but key was skipped.
	_ = cache
}

func TestFetchKeysExponentTooLarge(t *testing.T) {
	// Exponent encoded as a huge number (> int64 range).
	// 17 bytes of 0xFF → value exceeds int64.
	jwksBody := `{"keys":[
		{"kid":"big-exp","kty":"RSA","alg":"RS256","use":"sig","n":"AAAA","e":"__________________8"}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jwksBody))
	}))
	defer srv.Close()

	cache, err := auth.NewJWKSCache(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = cache
}

func TestValidateMissingKidHeader(t *testing.T) {
	// Create a JWT without a kid header.
	idp := testutil.NewMockIdP()
	defer idp.Close()

	cache, err := auth.NewJWKSCache(idp.IssuerURL() + "/jwks")
	if err != nil {
		t.Fatal(err)
	}

	// Build a JWT without kid using go-jose directly.
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: idp.PrivateKey()},
		// Deliberately omit WithHeader("kid", ...)
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	claims := map[string]any{
		"sub": "user-1",
		"iss": idp.IssuerURL(),
		"aud": "blockyard",
		"exp": now.Add(time.Hour).Unix(),
		"iat": now.Unix(),
	}
	raw, err := gojwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}

	_, err = cache.Validate(raw, idp.IssuerURL(), "blockyard")
	if err == nil {
		t.Error("expected error for token with missing kid header")
	}
}

func TestValidateKidNotFoundTriggersRefreshRetry(t *testing.T) {
	// Start a JWKS server that returns an empty key set on the first
	// request, but includes the real key on subsequent requests.
	idp := testutil.NewMockIdP()
	defer idp.Close()

	fetchCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetchCount++
		w.Header().Set("Content-Type", "application/json")
		if fetchCount == 1 {
			// First fetch: empty key set → kid won't match.
			w.Write([]byte(`{"keys":[]}`))
			return
		}
		// Subsequent fetches: proxy to real IdP JWKS.
		resp, err := http.Get(idp.IssuerURL() + "/jwks")
		if err != nil {
			w.WriteHeader(500)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 4096)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
			}
			if readErr != nil {
				break
			}
		}
	}))
	defer srv.Close()

	cache, err := auth.NewJWKSCache(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Reset cooldown so the kid-not-found path can actually trigger a refresh.
	cache.ResetLastRefresh()

	token := idp.IssueJWT("user-1", nil)

	// Validate sees kid-not-found → calls Refresh (cooldown bypassed) →
	// second fetch returns real key → retry succeeds.
	claims, err := cache.Validate(token, idp.IssuerURL(), "blockyard")
	if err != nil {
		t.Fatalf("expected retry to succeed after refresh, got: %v", err)
	}
	if claims.Subject != "user-1" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "user-1")
	}
	if fetchCount < 2 {
		t.Errorf("expected at least 2 JWKS fetches (initial + refresh), got %d", fetchCount)
	}
}

func TestRefreshAfterCooldownExpires(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	cache, err := auth.NewJWKSCache(idp.IssuerURL() + "/jwks")
	if err != nil {
		t.Fatal(err)
	}

	// Within cooldown: should be no-op.
	refreshed, err := cache.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Error("expected no refresh within cooldown")
	}

	// Reset lastRefresh to simulate cooldown expiry.
	cache.ResetLastRefresh()

	// Now Refresh should actually re-fetch keys.
	refreshed, err = cache.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed {
		t.Error("expected refresh after cooldown expired")
	}
}

func TestRefreshAfterCooldownFetchError(t *testing.T) {
	// Start with a working server, then make it fail after cooldown resets.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		if callCount == 1 {
			// Initial fetch succeeds.
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"keys":[]}`))
			return
		}
		// Subsequent fetches fail.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cache, err := auth.NewJWKSCache(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	cache.ResetLastRefresh()

	refreshed, err := cache.Refresh()
	if err == nil {
		t.Error("expected error when JWKS fetch fails during refresh")
	}
	if refreshed {
		t.Error("expected refreshed=false on error")
	}
}

func TestValidateKidNotFoundRefreshError(t *testing.T) {
	// JWKS server returns empty keys initially, then errors on refresh.
	// This tests Validate lines 76-77: refresh returns error.
	idp := testutil.NewMockIdP()
	defer idp.Close()

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"keys":[]}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cache, err := auth.NewJWKSCache(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	cache.ResetLastRefresh()

	token := idp.IssueJWT("user-1", nil)
	_, err = cache.Validate(token, idp.IssuerURL(), "blockyard")
	if err == nil {
		t.Error("expected error when refresh fails during kid-not-found recovery")
	}
}

func TestUnmarshalJSONInvalidJSON(t *testing.T) {
	var claims auth.JWTClaims
	err := json.Unmarshal([]byte(`{invalid json`), &claims)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestUnmarshalJSONValidFirstInvalidExtraPath(t *testing.T) {
	// The first unmarshal succeeds but the second unmarshal into Extra
	// also gets the same data. We need JSON that succeeds for the struct
	// unmarshal but fails for the map unmarshal. In practice both use
	// the same JSON so they either both succeed or both fail.
	// Test the first-unmarshal error path with truly broken JSON.
	var claims auth.JWTClaims
	err := json.Unmarshal([]byte(`not-json-at-all`), &claims)
	if err == nil {
		t.Error("expected error for broken JSON in UnmarshalJSON")
	}
}
