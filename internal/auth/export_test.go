package auth

import "time"

// ResetLastRefresh sets lastRefresh to the zero value, bypassing cooldown
// for testing purposes. Only available in test builds.
func (c *JWKSCache) ResetLastRefresh() {
	c.mu.Lock()
	c.lastRefresh = time.Time{}
	c.mu.Unlock()
}
