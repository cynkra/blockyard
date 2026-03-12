package proxy

import (
	"sync"
	"time"

	"github.com/coder/websocket"
)

// WsCache holds backend WebSocket connections after client disconnect.
// Keyed by session ID. Entries expire after a configurable TTL.
type WsCache struct {
	mu      sync.Mutex
	entries map[string]*cachedConn
}

type cachedConn struct {
	conn  *websocket.Conn
	timer *time.Timer
}

func NewWsCache() *WsCache {
	return &WsCache{entries: make(map[string]*cachedConn)}
}

// Cache stores a backend WebSocket connection with a TTL. When the TTL
// expires, the connection is closed and onExpire is called.
func (c *WsCache) Cache(sessionID string, conn *websocket.Conn, ttl time.Duration, onExpire func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict any existing entry for this session
	if existing, ok := c.entries[sessionID]; ok {
		existing.timer.Stop()
		existing.conn.CloseNow()
		delete(c.entries, sessionID)
	}

	timer := time.AfterFunc(ttl, func() {
		c.mu.Lock()
		entry, ok := c.entries[sessionID]
		if ok && entry.conn == conn {
			delete(c.entries, sessionID)
		}
		c.mu.Unlock()

		if ok {
			conn.CloseNow()
			onExpire()
		}
	})

	c.entries[sessionID] = &cachedConn{conn: conn, timer: timer}
}

// Take reclaims a cached connection. Returns nil if no entry exists.
// Stops the expiry timer.
func (c *WsCache) Take(sessionID string) *websocket.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[sessionID]
	if !ok {
		return nil
	}

	entry.timer.Stop()
	delete(c.entries, sessionID)
	return entry.conn
}
