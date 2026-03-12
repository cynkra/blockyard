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

// forwardClientHeaders copies relevant headers from the client request
// for the backend WebSocket dial. This ensures cookies, origin, and
// subprotocol negotiation are preserved end-to-end.
func forwardClientHeaders(r *http.Request) http.Header {
	h := http.Header{}
	for _, key := range []string{
		"Origin",
		"Cookie",
		"Sec-WebSocket-Protocol",
		"Sec-WebSocket-Extensions",
		"User-Agent",
		"X-Shiny-User",
		"X-Shiny-Groups",
		"X-Blockyard-Vault-Token",
	} {
		if v := r.Header.Get(key); v != "" {
			h.Set(key, v)
		}
	}
	return h
}

// wsMsg is a WebSocket message read from one side, to be written to
// the other.
type wsMsg struct {
	typ  websocket.MessageType
	data []byte
}

// backendReader owns a goroutine that reads from a backend WebSocket
// and sends messages to a channel. The reader can be transferred
// between shuttleWS instances via WsCache without losing messages or
// creating concurrent-reader races on the underlying connection.
//
// While cached (no consumer on msgs), the goroutine blocks on the
// channel send after reading one message. Ping/pong frames are
// handled internally by the coder/websocket library's readLoop as
// long as the goroutine is actively calling Read.
type backendReader struct {
	conn *websocket.Conn
	msgs chan wsMsg
	done chan struct{} // closed when the reader goroutine exits
	stop chan struct{} // close to signal the goroutine to stop
}

func newBackendReader(conn *websocket.Conn) *backendReader {
	br := &backendReader{
		conn: conn,
		msgs: make(chan wsMsg),
		done: make(chan struct{}),
		stop: make(chan struct{}),
	}
	go br.run()
	return br
}

func (br *backendReader) run() {
	defer close(br.done)
	for {
		typ, data, err := br.conn.Read(context.Background())
		if err != nil {
			slog.Debug("ws backend read done", "error", err)
			return
		}
		select {
		case br.msgs <- wsMsg{typ, data}:
		case <-br.stop:
			return
		}
	}
}

// Close stops the reader goroutine and closes the connection.
func (br *backendReader) Close() {
	select {
	case <-br.stop:
		// already stopped
	default:
		close(br.stop)
	}
	br.conn.CloseNow()
	<-br.done
}

// shuttleWS accepts a WebSocket from the client, connects to (or
// reclaims from cache) a backend WebSocket, and forwards messages
// bidirectionally until one side closes.
//
// Architecture: a dedicated backendReader goroutine reads from the
// backend and pushes messages to a channel. The main goroutine
// selects between client messages and backend messages, writing each
// to the other side. This design allows the backendReader to outlive
// individual client connections — when a client disconnects, the
// reader is cached and later handed to the next shuttleWS instance
// without concurrent-reader races or message loss.
//
// All Read/Write calls use context.Background() because the
// coder/websocket library closes connections when a Read's context
// is cancelled (via setupReadTimeout → c.close()).
func shuttleWS(
	w http.ResponseWriter,
	r *http.Request,
	addr, appName, sessionID string,
	cache *WsCache,
	srv *server.Server,
) {
	// Accept client WebSocket. InsecureSkipVerify disables the
	// library's origin check — origin policy is the backend's
	// responsibility, not the reverse proxy's.
	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Warn("ws accept failed", "error", err)
		return
	}
	// Remove the default 32KB read limit. A proxy should forward
	// whatever the endpoints exchange (e.g. base64 plot PNGs).
	clientConn.SetReadLimit(-1)

	// Obtain or create a backend reader.
	br := cache.Take(sessionID)
	if br == nil {
		backendURL := "ws://" + addr + stripAppPrefix(r.URL.Path, appName)
		backendConn, _, dialErr := websocket.Dial(context.Background(), backendURL, &websocket.DialOptions{
			HTTPHeader: forwardClientHeaders(r),
		})
		if dialErr != nil {
			slog.Warn("ws backend dial failed",
				"addr", addr, "error", dialErr)
			clientConn.Close(websocket.StatusInternalError,
				"backend connect failed")
			return
		}
		backendConn.SetReadLimit(-1)
		br = newBackendReader(backendConn)
	}

	// Client reader goroutine.
	clientMsgs := make(chan wsMsg)
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		for {
			typ, data, err := clientConn.Read(context.Background())
			if err != nil {
				slog.Debug("ws client read done", "error", err)
				return
			}
			select {
			case clientMsgs <- wsMsg{typ, data}:
			case <-br.done:
				// Backend reader gone — stop reading from client.
				return
			}
		}
	}()

	// Main select loop: shuttle messages between client and backend.
	cacheBackend := false
	for {
		select {
		case msg, ok := <-clientMsgs:
			if !ok {
				// Channel closed unexpectedly; treat as client done.
				cacheBackend = true
				goto done
			}
			if err := br.conn.Write(context.Background(), msg.typ, msg.data); err != nil {
				slog.Debug("ws backend write failed", "error", err)
				goto done
			}

		case msg := <-br.msgs:
			if err := clientConn.Write(context.Background(), msg.typ, msg.data); err != nil {
				slog.Debug("ws client write failed", "error", err)
				cacheBackend = true
				goto done
			}

		case <-clientDone:
			// Client reader exited (disconnect or error).
			cacheBackend = true
			goto done

		case <-br.done:
			// Backend reader exited (disconnect or error).
			goto done
		}
	}

done:
	if cacheBackend {
		// Client disconnected — cache the backend reader for
		// possible reconnect within the TTL.
		clientConn.CloseNow()

		slog.Debug("ws client disconnected, caching backend",
			"session_id", sessionID)
		cache.Cache(sessionID, br,
			srv.Config.Proxy.WsCacheTTL.Duration, func() {
				br.Close()
				workerID, ok := srv.Sessions.Get(sessionID)
				if !ok {
					return
				}
				srv.Sessions.Delete(sessionID)
				if srv.Sessions.CountForWorker(workerID) == 0 {
					slog.Info("ws cache expired, evicting idle worker",
						"worker_id", workerID, "session_id", sessionID)
					ops.EvictWorker(context.Background(), srv, workerID)
				}
			})
	} else {
		// Backend disconnected — close both sides.
		slog.Debug("ws backend disconnected",
			"session_id", sessionID)
		clientConn.Close(websocket.StatusGoingAway, "backend disconnected")
		br.Close()
	}
}
