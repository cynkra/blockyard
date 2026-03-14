package auth

import (
	"testing"
)

func TestCookieRoundTrip(t *testing.T) {
	key := DeriveSigningKey("test-secret")
	payload := &CookiePayload{Sub: "user-123", IssuedAt: 1700000000}

	encoded, err := payload.Encode(key)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := DecodeCookie(encoded, key)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Sub != "user-123" {
		t.Errorf("Sub = %q, want user-123", decoded.Sub)
	}
	if decoded.IssuedAt != 1700000000 {
		t.Errorf("IssuedAt = %d, want 1700000000", decoded.IssuedAt)
	}
}

func TestCookieTamperedPayload(t *testing.T) {
	key := DeriveSigningKey("test-secret")
	payload := &CookiePayload{Sub: "user-123", IssuedAt: 1700000000}

	encoded, err := payload.Encode(key)
	if err != nil {
		t.Fatal(err)
	}

	// Tamper with payload (flip a character).
	tampered := []byte(encoded)
	tampered[0] ^= 0x01
	_, err = DecodeCookie(string(tampered), key)
	if err == nil {
		t.Error("expected error for tampered payload")
	}
}

func TestCookieTamperedSignature(t *testing.T) {
	key := DeriveSigningKey("test-secret")
	payload := &CookiePayload{Sub: "user-123", IssuedAt: 1700000000}

	encoded, err := payload.Encode(key)
	if err != nil {
		t.Fatal(err)
	}

	// Tamper with the signature (last character).
	tampered := []byte(encoded)
	tampered[len(tampered)-1] ^= 0x01
	_, err = DecodeCookie(string(tampered), key)
	if err == nil {
		t.Error("expected error for tampered signature")
	}
}

func TestCookieWrongKey(t *testing.T) {
	key1 := DeriveSigningKey("secret-1")
	key2 := DeriveSigningKey("secret-2")
	payload := &CookiePayload{Sub: "user-123", IssuedAt: 1700000000}

	encoded, err := payload.Encode(key1)
	if err != nil {
		t.Fatal(err)
	}

	_, err = DecodeCookie(encoded, key2)
	if err == nil {
		t.Error("expected error when decoding with wrong key")
	}
}

func TestCookieMalformedNoDot(t *testing.T) {
	key := DeriveSigningKey("test-secret")
	_, err := DecodeCookie("nodothere", key)
	if err == nil {
		t.Error("expected error for cookie without dot separator")
	}
}

func TestCookieMalformedEmptySegments(t *testing.T) {
	key := DeriveSigningKey("test-secret")
	_, err := DecodeCookie(".", key)
	if err == nil {
		t.Error("expected error for cookie with empty segments")
	}
}

func TestUserSessionStoreSetGet(t *testing.T) {
	store := NewUserSessionStore()
	store.Set("alice", &UserSession{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		ExpiresAt:    1700001000,
	})

	sess := store.Get("alice")
	if sess == nil {
		t.Fatal("expected session for alice")
	}
	if sess.AccessToken != "at-1" {
		t.Errorf("AccessToken = %q", sess.AccessToken)
	}
}

func TestUserSessionStoreGetReturnsNilForMissing(t *testing.T) {
	store := NewUserSessionStore()
	if store.Get("nobody") != nil {
		t.Error("expected nil for missing session")
	}
}

func TestUserSessionStoreGetReturnsCopy(t *testing.T) {
	store := NewUserSessionStore()
	store.Set("alice", &UserSession{
		AccessToken: "at-1",
	})

	sess := store.Get("alice")
	sess.AccessToken = "mutated"

	original := store.Get("alice")
	if original.AccessToken != "at-1" {
		t.Error("access token mutation leaked through to stored session")
	}
}

func TestUserSessionStoreDelete(t *testing.T) {
	store := NewUserSessionStore()
	store.Set("alice", &UserSession{AccessToken: "at-1"})
	store.Delete("alice")

	if store.Get("alice") != nil {
		t.Error("expected session to be deleted")
	}
}

func TestUserSessionStoreUpdateTokens(t *testing.T) {
	store := NewUserSessionStore()
	store.Set("alice", &UserSession{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		ExpiresAt:    1000,
	})

	newRefresh := "rt-2"
	ok := store.UpdateTokens("alice", "at-2", &newRefresh, 2000)
	if !ok {
		t.Fatal("UpdateTokens returned false")
	}

	sess := store.Get("alice")
	if sess.AccessToken != "at-2" {
		t.Errorf("AccessToken = %q", sess.AccessToken)
	}
	if sess.RefreshToken != "rt-2" {
		t.Errorf("RefreshToken = %q", sess.RefreshToken)
	}
	if sess.ExpiresAt != 2000 {
		t.Errorf("ExpiresAt = %d", sess.ExpiresAt)
	}
}

func TestUserSessionStoreUpdateTokensNilRefresh(t *testing.T) {
	store := NewUserSessionStore()
	store.Set("alice", &UserSession{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		ExpiresAt:    1000,
	})

	ok := store.UpdateTokens("alice", "at-2", nil, 2000)
	if !ok {
		t.Fatal("UpdateTokens returned false")
	}

	sess := store.Get("alice")
	if sess.RefreshToken != "rt-1" {
		t.Errorf("RefreshToken should be unchanged, got %q", sess.RefreshToken)
	}
}

func TestUserSessionStoreUpdateTokensMissing(t *testing.T) {
	store := NewUserSessionStore()
	ok := store.UpdateTokens("nobody", "at-1", nil, 1000)
	if ok {
		t.Error("expected UpdateTokens to return false for missing session")
	}
}

func TestDeriveSigningKeyDeterministic(t *testing.T) {
	k1 := DeriveSigningKey("same-secret")
	k2 := DeriveSigningKey("same-secret")

	payload := &CookiePayload{Sub: "user", IssuedAt: 1}
	e1, _ := payload.Encode(k1)
	e2, _ := payload.Encode(k2)
	if e1 != e2 {
		t.Error("same secret should produce same key")
	}
}
