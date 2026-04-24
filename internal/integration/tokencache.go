package integration

import (
	"sync"
	"time"
)

// VaultTokenCache caches vault tokens keyed by user sub.
// Avoids calling the vault 's JWT login endpoint on every proxied request.
type VaultTokenCache struct {
	mu     sync.RWMutex
	tokens map[string]*cachedToken
}

type cachedToken struct {
	Token     string
	ExpiresAt time.Time
}

// NewVaultTokenCache creates an empty token cache.
func NewVaultTokenCache() *VaultTokenCache {
	return &VaultTokenCache{
		tokens: make(map[string]*cachedToken),
	}
}

// renewalBuffer is how far before expiry a token is considered stale.
// This avoids serving tokens that expire mid-request.
const renewalBuffer = 30 * time.Second

// Get returns a cached token if it exists and has at least renewalBuffer
// of remaining validity. Returns ("", false) on miss or near-expiry.
func (c *VaultTokenCache) Get(sub string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	t, ok := c.tokens[sub]
	if !ok {
		return "", false
	}
	if time.Now().Add(renewalBuffer).After(t.ExpiresAt) {
		return "", false
	}
	return t.Token, true
}

// Set stores a token with the given TTL.
func (c *VaultTokenCache) Set(sub string, token string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.tokens[sub] = &cachedToken{
		Token:     token,
		ExpiresAt: time.Now().Add(ttl),
	}
}

// Delete removes a cached token (e.g. on logout).
func (c *VaultTokenCache) Delete(sub string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.tokens, sub)
}

// Sweep removes all expired tokens from the cache.
func (c *VaultTokenCache) Sweep() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	removed := 0
	for sub, t := range c.tokens {
		if now.After(t.ExpiresAt) {
			delete(c.tokens, sub)
			removed++
		}
	}
	return removed
}
