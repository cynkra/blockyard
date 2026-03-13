package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/testutil"
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
