package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// SigningKey holds the derived HMAC key for cookie signing.
type SigningKey struct {
	key []byte
}

// DeriveSigningKey derives a signing key from a secret using HMAC
// with a domain separation string.
func DeriveSigningKey(secret string) *SigningKey {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("blockyard-cookie-signing"))
	return &SigningKey{key: mac.Sum(nil)}
}

// CookiePayload is the minimal payload encoded into the session cookie.
// Signed with HMAC-SHA256. All other session data lives server-side.
type CookiePayload struct {
	Sub      string `json:"sub"`
	IssuedAt int64  `json:"issued_at"`
}

// Encode serializes the payload and signs it with HMAC-SHA256.
// Format: base64url(json) + "." + base64url(hmac)
func (p *CookiePayload) Encode(key *SigningKey) (string, error) {
	jsonBytes, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal cookie payload: %w", err)
	}

	mac := hmac.New(sha256.New, key.key)
	mac.Write(jsonBytes)
	sig := mac.Sum(nil)

	payload := base64.RawURLEncoding.EncodeToString(jsonBytes)
	signature := base64.RawURLEncoding.EncodeToString(sig)
	return payload + "." + signature, nil
}

// DecodeCookie verifies the HMAC signature and deserializes the payload.
func DecodeCookie(value string, key *SigningKey) (*CookiePayload, error) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed cookie: missing separator")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode cookie payload: %w", err)
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode cookie signature: %w", err)
	}

	// Verify signature (constant-time comparison).
	mac := hmac.New(sha256.New, key.key)
	mac.Write(payloadBytes)
	expected := mac.Sum(nil)
	if !hmac.Equal(sigBytes, expected) {
		return nil, errors.New("invalid cookie signature")
	}

	var payload CookiePayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal cookie payload: %w", err)
	}
	return &payload, nil
}

// UserSession holds per-user session data stored server-side. Keyed by sub.
type UserSession struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // unix timestamp
}

// UserSessionStore is an in-memory session store protected by a RWMutex.
type UserSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*UserSession

	// Per-user refresh locks. Prevents concurrent token refreshes for
	// the same user — important because some IdPs (e.g. Keycloak with
	// refresh token rotation) invalidate the refresh token on first use.
	refreshMu    sync.Mutex
	refreshLocks map[string]*sync.Mutex
}

// NewUserSessionStore creates an empty session store.
func NewUserSessionStore() *UserSessionStore {
	return &UserSessionStore{
		sessions:     make(map[string]*UserSession),
		refreshLocks: make(map[string]*sync.Mutex),
	}
}

// RefreshLock returns the per-user mutex for token refresh. Creates
// one if it does not exist.
func (s *UserSessionStore) RefreshLock(sub string) *sync.Mutex {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	mu, ok := s.refreshLocks[sub]
	if !ok {
		mu = &sync.Mutex{}
		s.refreshLocks[sub] = mu
	}
	return mu
}

// Set inserts or replaces the session for a user.
func (s *UserSessionStore) Set(sub string, session *UserSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sub] = session
}

// Get looks up a user's session by sub. Returns nil if not found.
func (s *UserSessionStore) Get(sub string) *UserSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess := s.sessions[sub]
	if sess == nil {
		return nil
	}
	// Return a copy to avoid holding the lock.
	cp := *sess
	return &cp
}

// Delete removes a user's session (on logout).
func (s *UserSessionStore) Delete(sub string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sub)

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	delete(s.refreshLocks, sub)
}

// UpdateTokens updates the access/refresh tokens after a refresh.
// Returns false if the session does not exist.
func (s *UserSessionStore) UpdateTokens(sub, accessToken string, refreshToken *string, expiresAt int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sub]
	if !ok {
		return false
	}
	sess.AccessToken = accessToken
	if refreshToken != nil {
		sess.RefreshToken = *refreshToken
	}
	sess.ExpiresAt = expiresAt
	return true
}
