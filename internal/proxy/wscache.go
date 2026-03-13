package proxy

import (
	"sync"
	"time"
)

// WsCache holds backend readers after client disconnect. Keyed by
// session ID. Entries expire after a configurable TTL.
type WsCache struct {
	mu      sync.Mutex
	entries map[string]*cachedEntry
}

type cachedEntry struct {
	reader *backendReader
	timer  *time.Timer
}

func NewWsCache() *WsCache {
	return &WsCache{entries: make(map[string]*cachedEntry)}
}

// Cache stores a backend reader with a TTL. When the TTL expires,
// the reader is closed and onExpire is called.
func (c *WsCache) Cache(sessionID string, reader *backendReader, ttl time.Duration, onExpire func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict any existing entry for this session
	if existing, ok := c.entries[sessionID]; ok {
		existing.timer.Stop()
		existing.reader.Close()
		delete(c.entries, sessionID)
	}

	timer := time.AfterFunc(ttl, func() {
		c.mu.Lock()
		entry, ok := c.entries[sessionID]
		if ok && entry.reader == reader {
			delete(c.entries, sessionID)
		}
		c.mu.Unlock()

		if ok {
			onExpire()
		}
	})

	c.entries[sessionID] = &cachedEntry{reader: reader, timer: timer}
}

// Take reclaims a cached backend reader. Returns nil if no entry
// exists. Stops the expiry timer.
func (c *WsCache) Take(sessionID string) *backendReader {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[sessionID]
	if !ok {
		return nil
	}

	entry.timer.Stop()
	delete(c.entries, sessionID)
	return entry.reader
}
