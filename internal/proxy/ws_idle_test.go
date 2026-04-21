package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
)

// echoBackend returns an httptest server that echoes WebSocket messages.
func echoBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			if err := c.Write(context.Background(), typ, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newWSTestServer(t *testing.T, idleTTL time.Duration) *server.Server {
	t.Helper()
	srv := testColdstartServer(t)
	srv.Config.Proxy.SessionIdleTTL = config.Duration{Duration: idleTTL}
	return srv
}

func TestWSIdleTimeout(t *testing.T) {
	backend := echoBackend(t)
	srv := newWSTestServer(t, 100*time.Millisecond)

	sessionID := "idle-sess"
	srv.Sessions.Set(sessionID, session.Entry{
		WorkerID:   "w1",
		LastAccess: time.Now(),
	})
	srv.Registry.Set("w1", backend.Listener.Addr().String())

	cache := NewWsCache()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shuttleWS(w, r, backend.Listener.Addr().String(), "test-app", sessionID, "w1", 100, cache, srv)
	}))
	t.Cleanup(proxy.Close)

	conn, _, err := websocket.Dial(context.Background(),
		"ws://"+proxy.Listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	// No messages — idle timer should close the connection.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err = conn.Read(ctx)
	if err == nil {
		t.Fatal("expected read error after idle timeout")
	}
}

func TestWSIdleTimeoutResetOnMessage(t *testing.T) {
	backend := echoBackend(t)
	srv := newWSTestServer(t, 150*time.Millisecond)

	sessionID := "active-sess"
	srv.Sessions.Set(sessionID, session.Entry{
		WorkerID:   "w1",
		LastAccess: time.Now(),
	})
	srv.Registry.Set("w1", backend.Listener.Addr().String())

	cache := NewWsCache()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shuttleWS(w, r, backend.Listener.Addr().String(), "test-app", sessionID, "w1", 100, cache, srv)
	}))
	t.Cleanup(proxy.Close)

	conn, _, err := websocket.Dial(context.Background(),
		"ws://"+proxy.Listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	// Send messages at 60% of idle TTL to keep the connection alive.
	for i := 0; i < 4; i++ {
		time.Sleep(90 * time.Millisecond)
		if err := conn.Write(context.Background(), websocket.MessageText, []byte("ping")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		_, _, err := conn.Read(context.Background())
		if err != nil {
			t.Fatalf("read %d: unexpected close: %v", i, err)
		}
	}

	// Stop sending — idle timer should fire after 150ms.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err = conn.Read(ctx)
	if err == nil {
		t.Fatal("expected read error after idle timeout")
	}
}

func TestWSIdleTimeoutDisabled(t *testing.T) {
	backend := echoBackend(t)
	srv := newWSTestServer(t, 0) // disabled

	sessionID := "no-timeout-sess"
	srv.Sessions.Set(sessionID, session.Entry{
		WorkerID:   "w1",
		LastAccess: time.Now(),
	})
	srv.Registry.Set("w1", backend.Listener.Addr().String())

	cache := NewWsCache()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shuttleWS(w, r, backend.Listener.Addr().String(), "test-app", sessionID, "w1", 100, cache, srv)
	}))
	t.Cleanup(proxy.Close)

	conn, _, err := websocket.Dial(context.Background(),
		"ws://"+proxy.Listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	// Wait longer than any reasonable idle timeout would be.
	time.Sleep(300 * time.Millisecond)

	// Connection should still be alive — echo a message to verify.
	if err := conn.Write(context.Background(), websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal("write failed, connection should still be open:", err)
	}
	_, data, err := conn.Read(context.Background())
	if err != nil {
		t.Fatal("read failed, connection should still be open:", err)
	}
	if string(data) != "hello" {
		t.Fatalf("expected echo %q, got %q", "hello", data)
	}
}

func TestWSTouchDuringMessages(t *testing.T) {
	backend := echoBackend(t)
	// Use a long idle TTL so the timer doesn't interfere, but Touch
	// still fires (touchInterval is 30s, so we test with a short
	// touchInterval by sending many messages quickly — the first Touch
	// fires immediately since lastTouch starts at connection time and
	// we set LastAccess to the past).
	srv := newWSTestServer(t, 10*time.Second)

	sessionID := "touch-sess"
	old := time.Now().Add(-5 * time.Minute)
	srv.Sessions.Set(sessionID, session.Entry{
		WorkerID:   "w1",
		LastAccess: old,
	})
	srv.Registry.Set("w1", backend.Listener.Addr().String())

	cache := NewWsCache()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shuttleWS(w, r, backend.Listener.Addr().String(), "test-app", sessionID, "w1", 100, cache, srv)
	}))
	t.Cleanup(proxy.Close)

	conn, _, err := websocket.Dial(context.Background(),
		"ws://"+proxy.Listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	// Send a message to trigger the message path.
	if err := conn.Write(context.Background(), websocket.MessageText, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The first message should have called Touch because lastTouch
	// starts at time.Now() in shuttleWS and touchInterval is 30s.
	// However, our initial LastAccess is 5 minutes in the past, so
	// even without Touch the session record exists. The real test is
	// that after 30s+ of messages Touch would fire. For a unit test
	// we verify the session still exists (wasn't swept).
	entry, ok := srv.Sessions.Get(sessionID)
	if !ok {
		t.Fatal("session should still exist")
	}
	// LastAccess should NOT have been updated yet (touchInterval=30s,
	// only ~milliseconds have passed). This confirms Touch is
	// rate-limited as designed.
	if entry.LastAccess.After(old.Add(time.Second)) {
		t.Error("Touch should not have fired yet (rate-limited to 30s)")
	}
}
