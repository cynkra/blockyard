package proxy

import (
	"log/slog"
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
		slog.Debug("wscache: evicting existing entry on re-cache",
			"session_id", sessionID)
		existing.timer.Stop()
		existing.reader.Close()
		delete(c.entries, sessionID)
	}

	slog.Debug("wscache: caching backend reader",
		"session_id", sessionID, "ttl", ttl)

	timer := time.AfterFunc(ttl, func() {
		c.mu.Lock()
		entry, ok := c.entries[sessionID]
		matched := ok && entry.reader == reader
		if matched {
			slog.Debug("wscache: TTL expired, removing entry",
				"session_id", sessionID)
			delete(c.entries, sessionID)
		}
		c.mu.Unlock()

		if matched {
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
		slog.Debug("wscache: miss (no cached entry)",
			"session_id", sessionID)
		return nil
	}

	entry.timer.Stop()
	delete(c.entries, sessionID)
	slog.Debug("wscache: hit, reclaiming backend reader",
		"session_id", sessionID)
	return entry.reader
}
