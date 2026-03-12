package proxy

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"

	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/server"
)

// isWebSocketUpgrade checks whether the request is a WebSocket upgrade.
func isWebSocketUpgrade(r *http.Request) bool {
	return r.Header.Get("Upgrade") == "websocket"
}

// shuttleWS accepts a WebSocket from the client, connects to (or
// reclaims from cache) a backend WebSocket, and forwards messages
// bidirectionally until one side closes.
//
// If the client disconnects while the backend is still alive, the
// backend connection is cached for possible reconnect within the TTL.
// If the backend disconnects, both connections are closed.
func shuttleWS(
	w http.ResponseWriter,
	r *http.Request,
	addr, appName, sessionID string,
	cache *WsCache,
	srv *server.Server,
) {
	// Accept client WebSocket
	clientConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		slog.Warn("ws accept failed", "error", err)
		return
	}

	// Check cache for existing backend connection (reconnect case)
	backendConn := cache.Take(sessionID)
	if backendConn == nil {
		// No cached connection — dial the backend
		backendURL := "ws://" + addr + stripAppPrefix(r.URL.Path, appName)
		var dialErr error
		backendConn, _, dialErr = websocket.Dial(r.Context(), backendURL, nil)
		if dialErr != nil {
			slog.Warn("ws backend dial failed",
				"addr", addr, "error", dialErr)
			clientConn.Close(websocket.StatusInternalError,
				"backend connect failed")
			return
		}
	}

	// Bidirectional forwarding. Two goroutines: one for each direction.
	// The first goroutine to finish signals which side disconnected.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	const (
		sideClient  = 1
		sideBackend = 2
	)
	done := make(chan int, 2)

	// client → backend
	go func() {
		copyWS(ctx, backendConn, clientConn)
		done <- sideClient
		cancel()
	}()

	// backend → client
	go func() {
		copyWS(ctx, clientConn, backendConn)
		done <- sideBackend
		cancel()
	}()

	// Wait for first close — tells us which side disconnected
	firstClosed := <-done
	// Wait for second goroutine to finish
	<-done

	if firstClosed == sideClient {
		// Client disconnected, backend still alive — cache for reconnect
		slog.Debug("ws client disconnected, caching backend",
			"session_id", sessionID)
		cache.Cache(sessionID, backendConn,
			srv.Config.Proxy.WsCacheTTL.Duration, func() {
				// TTL expired without reconnect — clean up session
				workerID, ok := srv.Sessions.Get(sessionID)
				if !ok {
					return
				}
				srv.Sessions.Delete(sessionID)
				// If no other sessions reference this worker, it's idle — evict.
				if srv.Sessions.CountForWorker(workerID) == 0 {
					slog.Info("ws cache expired, evicting idle worker",
						"worker_id", workerID, "session_id", sessionID)
					ops.EvictWorker(context.Background(), srv, workerID)
				}
			})
	} else {
		// Backend disconnected — close both connections
		slog.Debug("ws backend disconnected",
			"session_id", sessionID)
		clientConn.Close(websocket.StatusGoingAway, "backend disconnected")
		backendConn.CloseNow()
	}
}

// copyWS reads messages from src and writes them to dst until an error
// occurs or the context is cancelled.
func copyWS(ctx context.Context, dst, src *websocket.Conn) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return
		}
	}
}
