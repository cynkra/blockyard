package auth_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
)

func TestSessionTokenRoundTrip(t *testing.T) {
	key := auth.DeriveSessionTokenKey("test-secret")
	now := time.Now().Unix()
	claims := &auth.SessionTokenClaims{
		Sub: "user-1",
		App: "app-x",
		Wid: "worker-42",
		Iat: now,
		Exp: now + 300,
	}

	token, err := auth.EncodeSessionToken(claims, key)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	got, err := auth.DecodeSessionToken(token, key)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Sub != claims.Sub {
		t.Errorf("Sub = %q, want %q", got.Sub, claims.Sub)
	}
	if got.App != claims.App {
		t.Errorf("App = %q, want %q", got.App, claims.App)
	}
	if got.Wid != claims.Wid {
		t.Errorf("Wid = %q, want %q", got.Wid, claims.Wid)
	}
	if got.Iat != claims.Iat {
		t.Errorf("Iat = %d, want %d", got.Iat, claims.Iat)
	}
	if got.Exp != claims.Exp {
		t.Errorf("Exp = %d, want %d", got.Exp, claims.Exp)
	}
}

func TestSessionTokenExpired(t *testing.T) {
	key := auth.DeriveSessionTokenKey("test-secret")
	claims := &auth.SessionTokenClaims{
		Sub: "user-1",
		App: "app-x",
		Wid: "worker-42",
		Iat: time.Now().Unix() - 600,
		Exp: time.Now().Unix() - 300,
	}

	token, err := auth.EncodeSessionToken(claims, key)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	_, err = auth.DecodeSessionToken(token, key)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %q, want it to mention expired", err.Error())
	}
}

func TestSessionTokenTampered(t *testing.T) {
	key := auth.DeriveSessionTokenKey("test-secret")
	now := time.Now().Unix()
	claims := &auth.SessionTokenClaims{
		Sub: "user-1",
		App: "app-x",
		Wid: "worker-42",
		Iat: now,
		Exp: now + 300,
	}

	token, err := auth.EncodeSessionToken(claims, key)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Replace a character in the payload portion (before the dot) to
	// produce a different but still valid base64url string.
	parts := strings.SplitN(token, ".", 2)
	payload := []byte(parts[0])
	if payload[len(payload)-1] == 'A' {
		payload[len(payload)-1] = 'B'
	} else {
		payload[len(payload)-1] = 'A'
	}
	tampered := string(payload) + "." + parts[1]

	_, err = auth.DecodeSessionToken(tampered, key)
	if err == nil {
		t.Fatal("expected error for tampered token, got nil")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("error = %q, want it to mention signature", err.Error())
	}
}

func TestSessionTokenWrongKey(t *testing.T) {
	keyA := auth.DeriveSessionTokenKey("secret-a")
	keyB := auth.DeriveSessionTokenKey("secret-b")
	now := time.Now().Unix()
	claims := &auth.SessionTokenClaims{
		Sub: "user-1",
		App: "app-x",
		Wid: "worker-42",
		Iat: now,
		Exp: now + 300,
	}

	token, err := auth.EncodeSessionToken(claims, keyA)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	_, err = auth.DecodeSessionToken(token, keyB)
	if err == nil {
		t.Fatal("expected error when decoding with wrong key, got nil")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("error = %q, want it to mention signature", err.Error())
	}
}

func TestSessionTokenKeyDomainSeparation(t *testing.T) {
	secret := "shared-secret"
	sessionKey := auth.DeriveSessionTokenKey(secret)
	cookieKey := auth.DeriveSigningKey(secret)
	now := time.Now().Unix()

	// Encode a session token with the session token key.
	claims := &auth.SessionTokenClaims{
		Sub: "user-1",
		App: "app-x",
		Wid: "worker-42",
		Iat: now,
		Exp: now + 300,
	}
	token, err := auth.EncodeSessionToken(claims, sessionKey)
	if err != nil {
		t.Fatalf("encode session token: %v", err)
	}

	// Try to decode the session token using the cookie signing key.
	_, err = auth.DecodeSessionToken(token, cookieKey)
	if err == nil {
		t.Fatal("session token must not decode with cookie signing key")
	}

	// Encode a cookie with the cookie signing key.
	cp := &auth.CookiePayload{Sub: "user-1", IssuedAt: now}
	cookie, err := cp.Encode(cookieKey)
	if err != nil {
		t.Fatalf("encode cookie: %v", err)
	}

	// Try to decode the cookie using the session token key.
	_, err = auth.DecodeCookie(cookie, sessionKey)
	if err == nil {
		t.Fatal("cookie must not decode with session token key")
	}
}
