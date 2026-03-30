package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SessionTokenClaims is the payload of a session reference token.
// Issued by the proxy on each request to shared containers. The app
// exchanges this for real credentials via the credential exchange API.
type SessionTokenClaims struct {
	Sub string `json:"sub"` // user subject
	App string `json:"app"` // app ID
	Wid string `json:"wid"` // worker ID
	Iat int64  `json:"iat"` // issued at (unix seconds)
	Exp int64  `json:"exp"` // expiry (unix seconds)
}

// SessionTokenTTL is the validity window for session reference tokens.
const SessionTokenTTL = 5 * time.Minute

// DeriveSessionTokenKey derives a signing key for session tokens.
// Uses a different domain string than cookie signing to prevent
// cross-protocol token confusion.
func DeriveSessionTokenKey(secret string) *SigningKey {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("blockyard-session-token"))
	return &SigningKey{key: mac.Sum(nil)}
}

// EncodeSessionToken serializes and signs a session reference token.
// Format: base64url(json) + "." + base64url(hmac)
func EncodeSessionToken(claims *SessionTokenClaims, key *SigningKey) (string, error) {
	jsonBytes, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal session token: %w", err)
	}

	mac := hmac.New(sha256.New, key.key)
	mac.Write(jsonBytes)
	sig := mac.Sum(nil)

	payload := base64.RawURLEncoding.EncodeToString(jsonBytes)
	signature := base64.RawURLEncoding.EncodeToString(sig)
	return payload + "." + signature, nil
}

// DecodeSessionToken verifies the HMAC signature and deserializes the
// claims. Returns an error if the signature is invalid or the token
// has expired.
func DecodeSessionToken(token string, key *SigningKey) (*SessionTokenClaims, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed session token: missing separator")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode session token payload: %w", err)
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode session token signature: %w", err)
	}

	mac := hmac.New(sha256.New, key.key)
	mac.Write(payloadBytes)
	expected := mac.Sum(nil)
	if !hmac.Equal(sigBytes, expected) {
		return nil, errors.New("invalid session token signature")
	}

	var claims SessionTokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("unmarshal session token: %w", err)
	}

	now := time.Now().Unix()
	const clockSkew = 60 // allow 60s clock drift

	if now > claims.Exp {
		return nil, errors.New("session token expired")
	}
	if claims.Iat > now+clockSkew {
		return nil, errors.New("session token issued in the future")
	}
	if claims.Exp <= claims.Iat {
		return nil, errors.New("session token expiry before issuance")
	}

	return &claims, nil
}
