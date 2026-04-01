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

// testBackendReader creates a test WebSocket server, dials it, and
// returns a backendReader wrapping the client-side connection.
func testBackendReader(t *testing.T) *backendReader {
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
	br := newBackendReader(conn)
	t.Cleanup(func() { br.Close() })
	return br
}

func TestCacheTakeBeforeExpiry(t *testing.T) {
	cache := NewWsCache()
	br := testBackendReader(t)

	var expired atomic.Bool
	cache.Cache("sess-1", br, 5*time.Second, func() {
		expired.Store(true)
	})

	got := cache.Take("sess-1")
	if got == nil {
		t.Fatal("expected non-nil reader from Take")
		return
	}
	if got != br {
		t.Error("expected same reader back")
	}

	// Ensure onExpire was not called
	time.Sleep(50 * time.Millisecond)
	if expired.Load() {
		t.Error("onExpire should not have been called")
	}
}

func TestCacheExpiry(t *testing.T) {
	cache := NewWsCache()
	br := testBackendReader(t)

	var expired atomic.Bool
	cache.Cache("sess-1", br, 10*time.Millisecond, func() {
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
	br1 := testBackendReader(t)
	br2 := testBackendReader(t)

	cache.Cache("sess-1", br1, 5*time.Second, func() {})
	cache.Cache("sess-1", br2, 5*time.Second, func() {})

	got := cache.Take("sess-1")
	if got != br2 {
		t.Error("expected second reader")
	}
}

func TestTakeNonexistent(t *testing.T) {
	cache := NewWsCache()
	got := cache.Take("no-such-session")
	if got != nil {
		t.Error("expected nil for nonexistent session")
	}
}
