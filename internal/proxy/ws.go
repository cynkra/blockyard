package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/server"
)

// wsOriginPatterns derives WebSocket origin patterns from the external URL.
// Rejects all cross-origin requests when the URL is empty or unparseable
// to prevent cross-site WebSocket hijacking in misconfigured deployments.
func wsOriginPatterns(externalURL string) []string {
	if externalURL == "" {
		slog.Warn("ws: external_url not configured, rejecting all cross-origin WebSocket requests")
		return []string{}
	}
	u, err := url.Parse(externalURL)
	if err != nil || u.Host == "" {
		slog.Warn("ws: external_url unparseable, rejecting all cross-origin WebSocket requests",
			"external_url", externalURL)
		return []string{}
	}
	// Allow both http and https variants of the configured host.
	return []string{u.Host}
}

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
		"X-Shiny-Access",
		"X-Shiny-Groups",
		"X-Blockyard-Vault-Token",
		"X-Blockyard-Session-Token",
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
// When session_idle_ttl > 0, an idle timer closes the connection if
// no application-level messages are exchanged within the configured
// duration. Ping/pong frames are handled internally by coder/websocket
// and do not reset the timer.
//
// All Read/Write calls use context.Background() because the
// coder/websocket library closes connections when a Read's context
// is cancelled (via setupReadTimeout → c.close()).
func shuttleWS(
	w http.ResponseWriter,
	r *http.Request,
	addr, appName, sessionID, workerID string,
	maxSessionsPerWorker int,
	cache *WsCache,
	srv *server.Server,
) {
	// Enforce max_sessions_per_worker at handshake time. A session in
	// blockyard is one active WebSocket, so this is where the limit is
	// checked. Reject before Accept so the browser sees a 503 rather
	// than a successful Upgrade followed by an immediate close.
	if !srv.WsConns.TryInc(workerID, maxSessionsPerWorker) {
		slog.Info("ws rejected: worker at session capacity", //nolint:gosec // G706: slog structured logging handles this
			"app", appName, "worker_id", workerID,
			"max_sessions_per_worker", maxSessionsPerWorker)
		http.Error(w, "app at capacity", http.StatusServiceUnavailable)
		return
	}
	srv.Metrics.SessionsActive.Inc()
	defer func() {
		srv.WsConns.Dec(workerID)
		srv.Metrics.SessionsActive.Dec()
	}()

	// Accept client WebSocket. Origin is restricted to the configured
	// external_url host. Auth is enforced at the session/cookie layer.
	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: wsOriginPatterns(srv.Config.Server.ExternalURL),
	})
	if err != nil {
		slog.Warn("ws accept failed", "error", err)
		return
	}
	// Raise the default 32KB read limit. A proxy must forward larger
	// payloads (e.g. base64 plot PNGs) but we cap at 128 MiB to
	// prevent memory-exhaustion DoS.
	const maxWSMessageSize = 128 << 20 // 128 MiB
	clientConn.SetReadLimit(maxWSMessageSize)

	// Obtain or create a backend reader.
	br := cache.Take(sessionID)
	if br == nil {
		// Backend workers are local containers on the Docker network;
		// they don't serve TLS, so ws:// is correct for internal traffic.
		backendPath := stripAppPrefix(r.URL.Path, appName)
		if r.URL.RawQuery != "" {
			backendPath += "?" + r.URL.RawQuery
		}
		backendURL := "ws://" + addr + backendPath
		dialCtx, dialCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dialCancel()
		backendConn, _, dialErr := websocket.Dial(dialCtx, backendURL, &websocket.DialOptions{
			HTTPHeader: forwardClientHeaders(r),
		})
		if dialErr != nil {
			slog.Warn("ws backend dial failed", //nolint:gosec // G706: slog structured logging handles this
				"addr", addr, "error", dialErr)
			clientConn.Close(websocket.StatusInternalError,
				"backend connect failed")
			return
		}
		backendConn.SetReadLimit(maxWSMessageSize)
		br = newBackendReader(backendConn)
	}

	// Client reader goroutine.
	clientMsgs := make(chan wsMsg)
	clientDone := make(chan struct{})
	// clientCloseStatus captures the WebSocket close code sent by the
	// peer. Written before clientDone is closed, read only after
	// <-clientDone, so no data race.
	var clientCloseStatus websocket.StatusCode = -1
	go func() { //nolint:gosec // G118: intentional background relay, outlives request
		defer close(clientDone)
		for {
			typ, data, err := clientConn.Read(context.Background())
			if err != nil {
				clientCloseStatus = websocket.CloseStatus(err)
				slog.Debug("ws client read done", "error", err,
					"close_status", clientCloseStatus)
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

	// Optional hard cap on session duration. When set, the connection
	// is closed after the configured lifetime regardless of activity.
	var lifetimeC <-chan time.Time
	if maxLife := srv.Config.Proxy.SessionMaxLifetime.Duration; maxLife > 0 {
		t := time.NewTimer(maxLife)
		defer t.Stop()
		lifetimeC = t.C
	}

	// Optional idle timeout. When set, the connection is closed if no
	// application-level messages are exchanged within the duration.
	// Ping/pong frames are handled by coder/websocket internally and
	// never appear here, so only real user activity resets the timer.
	idleTTL := srv.Config.Proxy.SessionIdleTTL.Duration
	var idleC <-chan time.Time
	var idleTimer *time.Timer
	if idleTTL > 0 {
		idleTimer = time.NewTimer(idleTTL)
		defer idleTimer.Stop()
		idleC = idleTimer.C
	}

	// Rate-limit Touch calls to avoid lock contention on the session
	// store. LastAccess may be up to touchInterval stale, which is
	// fine for a sweep running on 15s ticks with TTLs in minutes/hours.
	const touchInterval = 30 * time.Second
	lastTouch := time.Now()

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
			slog.Log(context.Background(), config.LevelTrace,
				"ws: client→backend", "session_id", sessionID,
				"type", msg.typ, "len", len(msg.data))
			if err := br.conn.Write(context.Background(), msg.typ, msg.data); err != nil {
				slog.Debug("ws backend write failed", "error", err)
				goto done
			}
			if idleTimer != nil {
				idleTimer.Reset(idleTTL)
			}
			if time.Since(lastTouch) > touchInterval {
				srv.Sessions.Touch(sessionID)
				lastTouch = time.Now()
			}

		case msg := <-br.msgs:
			slog.Log(context.Background(), config.LevelTrace,
				"ws: backend→client", "session_id", sessionID,
				"type", msg.typ, "len", len(msg.data))
			if err := clientConn.Write(context.Background(), msg.typ, msg.data); err != nil {
				slog.Debug("ws client write failed", "error", err)
				cacheBackend = true
				goto done
			}
			if idleTimer != nil {
				idleTimer.Reset(idleTTL)
			}
			if time.Since(lastTouch) > touchInterval {
				srv.Sessions.Touch(sessionID)
				lastTouch = time.Now()
			}

		case <-clientDone:
			// Client reader exited. Only cache the backend reader when
			// the disconnect was abnormal (network error, no close frame).
			// A clean close — 1000 (Normal) or 1001 (Going Away) — means
			// the browser deliberately left (page reload, navigation, tab
			// close). In that case the Shiny client state is gone, so a
			// cached backend connection would deliver stale messages that
			// the new page's freshly-initialized Shiny JS cannot handle.
			cs := clientCloseStatus
			cacheBackend = cs != websocket.StatusNormalClosure &&
				cs != websocket.StatusGoingAway
			goto done

		case <-br.done:
			// Backend reader exited (disconnect or error).
			goto done

		case <-lifetimeC:
			// Session exceeded max lifetime — close both sides.
			slog.Info("ws session max lifetime reached", "session_id", sessionID) //nolint:gosec // G706: slog structured logging handles this
			goto done

		case <-idleC:
			// No application-level messages for session_idle_ttl — close.
			slog.Info("ws session idle timeout", "session_id", sessionID, //nolint:gosec // G706: slog structured logging handles this
				"idle_ttl", idleTTL)
			goto done
		}
	}

done:
	if cacheBackend {
		// Client disconnected — cache the backend reader for
		// possible reconnect within the TTL.
		clientConn.CloseNow()

		slog.Debug("ws client disconnected, caching backend", //nolint:gosec // G706: slog structured logging handles this
			"session_id", sessionID)
		cache.Cache(sessionID, br,
			srv.Config.Proxy.WsCacheTTL.Duration, func() {
				br.Close()
				// TTL expired without reconnect — clean up session
				entry, ok := srv.Sessions.Get(sessionID)
				if !ok {
					return
				}
				srv.Sessions.Delete(sessionID)
				// If no active WebSocket remains on this worker, mark idle.
				// The idle worker reaper will evict it after idle_worker_timeout
				// if no new sessions arrive (and it's not the last worker for the app).
				if srv.WsConns.Count(entry.WorkerID) == 0 {
					srv.Workers.SetIdleSince(entry.WorkerID, time.Now())
				}
			})
	} else {
		// Backend disconnected — close both sides.
		slog.Debug("ws backend disconnected", //nolint:gosec // G706: slog structured logging handles this
			"session_id", sessionID)
		clientConn.Close(websocket.StatusGoingAway, "backend disconnected")
		br.Close()
	}
}
