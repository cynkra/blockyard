package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dialTestWS creates a test WebSocket server and dials it, returning the
// client-side connection. The server-side connection is closed on cleanup.
func dialTestWS(t *testing.T) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Keep alive until test ends
		<-r.Context().Done()
		c.CloseNow()
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.Dial(context.Background(), "ws://"+srv.Listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func TestCacheTakeBeforeExpiry(t *testing.T) {
	cache := NewWsCache()
	conn := dialTestWS(t)

	var expired atomic.Bool
	cache.Cache("sess-1", conn, 5*time.Second, func() {
		expired.Store(true)
	})

	got := cache.Take("sess-1")
	if got == nil {
		t.Fatal("expected non-nil connection from Take")
	}
	if got != conn {
		t.Error("expected same connection back")
	}

	// Ensure onExpire was not called
	time.Sleep(50 * time.Millisecond)
	if expired.Load() {
		t.Error("onExpire should not have been called")
	}
}

func TestCacheExpiry(t *testing.T) {
	cache := NewWsCache()
	conn := dialTestWS(t)

	var expired atomic.Bool
	cache.Cache("sess-1", conn, 10*time.Millisecond, func() {
		expired.Store(true)
	})

	// Wait for expiry
	time.Sleep(100 * time.Millisecond)

	got := cache.Take("sess-1")
	if got != nil {
		t.Error("expected nil after expiry")
	}
	if !expired.Load() {
		t.Error("onExpire should have been called")
	}
}

func TestCacheEvictsExisting(t *testing.T) {
	cache := NewWsCache()
	conn1 := dialTestWS(t)
	conn2 := dialTestWS(t)

	cache.Cache("sess-1", conn1, 5*time.Second, func() {})
	cache.Cache("sess-1", conn2, 5*time.Second, func() {})

	got := cache.Take("sess-1")
	if got != conn2 {
		t.Error("expected second connection")
	}
}

func TestTakeNonexistent(t *testing.T) {
	cache := NewWsCache()
	got := cache.Take("no-such-session")
	if got != nil {
		t.Error("expected nil for nonexistent session")
	}
}
