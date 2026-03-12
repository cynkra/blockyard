# Phase 0-5: HTTP/WebSocket Reverse Proxy

Route user traffic to Shiny app containers. This is the layer that makes
deployed apps accessible — without it, the server can manage containers
but nobody can use them. Covers session routing, HTTP/WS forwarding,
cold-start holding, and WebSocket session caching with message buffering.

## Deliverables

1. `wsSession` — persistent backend WebSocket reader with message
   buffering for client reconnects
2. `WsCache` — TTL-based cache for `wsSession` entries
3. Session cookie helpers — set and read `blockyard_session` cookie
4. Cold-start holding — hold initial request while worker starts, poll
   health until ready or `worker_start_timeout` expires
5. HTTP reverse proxy — forward requests to the correct worker
6. WebSocket reverse proxy — upgrade handling, bidirectional forwarding,
   session caching with message buffering
7. Proxy handler — catch-all for `/app/{name}/` routes, session
   management, cold-start orchestration, active bundle guard
8. Trailing-slash redirect — `/app/{name}` → `/app/{name}/`
9. Path prefix stripping — remove `/app/{name}` before forwarding
10. Mock backend `SetWSHandler` — extend mock for WS integration tests
11. Router composition — proxy routes alongside API routes in `NewRouter`
12. Fix: enforce active bundle invariant when clearing `active_bundle`
13. Integration tests — end-to-end HTTP and WebSocket proxying through
    mock backend

## What's already done

Phase 0-1 delivered:

- `session.Store` — session-to-worker mapping with `Get`, `Set`,
  `Delete`, `DeleteByWorker`, `CountForWorker`
- `registry.Registry` — worker-to-address mapping with `Get`, `Set`,
  `Delete`
- `server.Server` struct with `Workers`, `Sessions`, `Registry`,
  `Tasks`, `LogStore`, `Config`, `Backend`, `DB`
- `server.WorkerMap` with `Get`, `Set`, `Delete`, `Count`, `ForApp`,
  `CountForApp`
- `config.ProxyConfig` with `WsCacheTTL`, `WorkerStartTimeout`,
  `MaxWorkers`, `HealthInterval`, `LogRetention`
- `backend.Backend` interface with `Spawn`, `Stop`, `HealthCheck`,
  `Logs`, `Addr`, `Build`, `ListManaged`, `RemoveResource`

Phase 0-2 delivered:

- `DockerBackend` with full `Backend` implementation
- `MockBackend` with httptest workers and configurable health/build
  responses

Phase 0-3 delivered:

- Bundle storage, restore pipeline, retention cleanup
- chi router (`NewRouter`), bearer token auth middleware, `/healthz`
- `POST /api/v1/apps/{id}/bundles`, `GET /api/v1/apps/{id}/bundles`
- Task status and log streaming endpoints
- `NewServer()` constructor, `main.go` with HTTP server and graceful
  shutdown
- `bundle.NewBundlePaths()` shared path constructor

Phase 0-4 delivered:

- App CRUD endpoints (create, list, get, update, delete)
- App lifecycle endpoints (start, stop)
- App log streaming (`GET /api/v1/apps/{id}/logs`)
- `resolveApp` helper — resolve `{id}` by UUID or name
- `stopAppWorkers` helper — stop all workers for an app
- `appResponse` with derived `status` field
- Error response helpers (`badRequest`, `notFound`, `conflict`,
  `serviceUnavailable`, `serverError`)
- `WorkerMap.ForApp(appID)` returns worker IDs for an app

## Step-by-step

### Step 1: wsSession — persistent backend reader with message buffering

`internal/proxy/wssession.go` — owns a backend WebSocket connection for
its entire lifetime. A persistent reader goroutine reads from the
backend and either forwards to an attached client or buffers messages
for replay on reconnect.

**Why not cache a raw `*websocket.Conn`.** `coder/websocket` closes the
underlying connection when a `Read`'s context is cancelled — there is no
way to interrupt a blocked `Read` without destroying the connection. A
naive two-goroutine shuttle with a shared context would kill the backend
connection when `cancel()` is called after the client disconnects. The
`wsSession` model avoids this by making the reader goroutine the sole,
permanent owner of `backendConn.Read` — no context cancellation is ever
needed.

**Message buffering.** While no client is attached (between disconnect
and reconnect), messages from the backend are buffered. On reconnect,
buffered messages are replayed before switching to live forwarding. This
is critical for mobile and flaky connections where brief disconnects are
frequent — without buffering, any backend messages sent during the
disconnect window (UI updates, reactive output) are silently lost. The
Shiny R process doesn't know the client missed them and won't re-send.

```go
package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/coder/websocket"
)

// wsSession owns a backend WebSocket connection for its entire lifetime.
// A persistent reader goroutine reads from the backend and either
// forwards to an attached client or buffers messages for later replay.
type wsSession struct {
	backendConn *websocket.Conn

	mu     sync.Mutex
	client *websocket.Conn // nil when no client is attached
	buffer []wsMsg         // messages received while detached
	closed bool            // true after backend disconnects
}

type wsMsg struct {
	typ  websocket.MessageType
	data []byte
}

const maxBufferedMessages = 1000

func newSession(backendConn *websocket.Conn) *wsSession {
	s := &wsSession{
		backendConn: backendConn,
	}
	go s.readLoop()
	return s
}

// readLoop is the sole reader of backendConn. It runs for the
// connection's entire lifetime — no other goroutine ever calls
// backendConn.Read. Messages are forwarded to the attached client
// or buffered if no client is attached.
func (s *wsSession) readLoop() {
	for {
		typ, data, err := s.backendConn.Read(context.Background())
		if err != nil {
			s.mu.Lock()
			s.closed = true
			client := s.client
			s.mu.Unlock()
			if client != nil {
				client.Close(websocket.StatusGoingAway,
					"backend disconnected")
			}
			return
		}

		s.mu.Lock()
		client := s.client
		if client == nil {
			// No client attached — buffer for replay on reconnect.
			// If the buffer is full, the session is unrecoverable:
			// dropping messages corrupts the Shiny protocol stream.
			// Close the backend connection and let the next reconnect
			// fail cleanly — the user reloads and gets a fresh session.
			if len(s.buffer) >= maxBufferedMessages {
				s.closed = true
				s.mu.Unlock()
				s.backendConn.CloseNow()
				return
			}
			s.buffer = append(s.buffer, wsMsg{typ, data})
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()

		if err := client.Write(context.Background(), typ, data); err != nil {
			// Client write failed — detach. The shuttle goroutine
			// will also notice and call Detach explicitly.
			s.mu.Lock()
			if s.client == client {
				s.client = nil
			}
			s.mu.Unlock()
		}
	}
}

// Attach connects a client to this session. Replays any buffered
// messages first, then sets the client for live forwarding. Holds
// the lock during replay to ensure correct ordering between buffered
// and live messages — the reader goroutine blocks until replay is
// complete. This is acceptable because replay is bounded by
// maxBufferedMessages and only happens on reconnect (infrequent).
func (s *wsSession) Attach(client *websocket.Conn) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return errors.New("backend disconnected")
	}

	for _, msg := range s.buffer {
		if err := client.Write(context.Background(), msg.typ, msg.data); err != nil {
			return fmt.Errorf("replay buffered message: %w", err)
		}
	}
	s.buffer = nil
	s.client = client
	return nil
}

// Detach removes the client from this session. The reader goroutine
// switches to buffering mode.
func (s *wsSession) Detach() {
	s.mu.Lock()
	s.client = nil
	s.mu.Unlock()
}

// IsClosed returns true if the backend connection has disconnected.
func (s *wsSession) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close shuts down the backend connection. The reader goroutine exits
// when its next Read fails.
func (s *wsSession) Close() {
	s.backendConn.CloseNow()
}
```

**Connection ownership model.** `coder/websocket` uses separate
internal mutexes for Read and Write, so concurrent Read + Write on the
same connection is safe. The reader goroutine is the sole caller of
`backendConn.Read`. `shuttleWS` (in the handler's goroutine) is the
sole caller of `backendConn.Write` (client→backend direction). The
reader goroutine writes to `clientConn`; `shuttleWS` reads from
`clientConn`. No concurrent Read+Read or Write+Write on any connection.

**Buffer overflow.** When the buffer reaches `maxBufferedMessages`
(1000), the session closes the backend connection and marks itself
closed. Dropping messages — whether oldest or newest — silently
corrupts the Shiny protocol stream (messages are ordered protocol
events, not independent UI snapshots). A clean close is the honest
failure mode: the user reloads and gets a fresh session.

**Tests:**

- `TestSessionForwardToClient` — attach a client, send a message from
  backend, verify client receives it
- `TestSessionBufferWhileDetached` — send messages with no client
  attached, attach, verify replay
- `TestSessionBufferOverflowCloses` — send `maxBufferedMessages + 1`
  with no client, verify `IsClosed()` is true
- `TestSessionBackendDisconnect` — close backend, verify `IsClosed()`
  and attached client receives close frame
- `TestSessionAttachAfterClose` — verify `Attach` returns error

### Step 2: WsCache

`internal/proxy/wscache.go` — holds `wsSession` entries after client
disconnect, keyed by session ID. Entries expire after a configurable
TTL. The TTL is set once at construction time.

```go
package proxy

import (
	"sync"
	"time"
)

// WsCache holds wsSession entries after client disconnect. Keyed by
// session ID. Entries expire after the TTL set at construction time.
type WsCache struct {
	mu      sync.Mutex
	entries map[string]*cachedSession
	ttl     time.Duration
}

type cachedSession struct {
	session *wsSession
	timer   *time.Timer
}

func NewWsCache(ttl time.Duration) *WsCache {
	return &WsCache{
		entries: make(map[string]*cachedSession),
		ttl:     ttl,
	}
}

// Put stores a detached session. When the TTL expires without a
// reconnect, the session is closed and onExpire is called.
func (c *WsCache) Put(sessionID string, s *wsSession, onExpire func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict any existing entry for this session
	if existing, ok := c.entries[sessionID]; ok {
		existing.timer.Stop()
		existing.session.Close()
		delete(c.entries, sessionID)
	}

	timer := time.AfterFunc(c.ttl, func() {
		c.mu.Lock()
		entry, ok := c.entries[sessionID]
		removed := ok && entry.session == s
		if removed {
			delete(c.entries, sessionID)
		}
		c.mu.Unlock()

		if removed {
			s.Close()
			onExpire()
		}
	})

	c.entries[sessionID] = &cachedSession{session: s, timer: timer}
}

// Take reclaims a cached session. Returns nil if no entry exists.
// Stops the expiry timer.
func (c *WsCache) Take(sessionID string) *wsSession {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[sessionID]
	if !ok {
		return nil
	}

	entry.timer.Stop()
	delete(c.entries, sessionID)
	return entry.session
}
```

`Put` is called when the client end of a WebSocket closes but the
backend connection is still alive. `Take` is called on reconnect — if
the client opens a new WebSocket to the same app within the TTL, the
cached session is reclaimed with its buffered messages.

The `onExpire` callback is called when the TTL fires without a
reconnect. The proxy uses this to clean up the session mapping and
(once phase 0-6 lands `evict_worker`) evict the worker if no other
sessions reference it.

`time.AfterFunc` runs the callback in its own goroutine, so the lock
inside the callback does not deadlock with `Put` or `Take`. The
`removed` flag ensures `onExpire` is only called when the entry was
actually removed by the timer — not when another `Put` replaced it.

**Tests:**

- `TestCacheTakeBeforeExpiry` — put a session, take it back,
  verify non-nil and timer stopped
- `TestCacheExpiry` — put with short TTL (10ms), sleep, verify
  `Take` returns nil and `onExpire` was called
- `TestCacheEvictsExisting` — put twice for same session, verify
  first session is closed
- `TestTakeNonexistent` — take from empty cache, verify nil
- `TestCacheExpiryAfterReplace` — put session A, put session B for
  same key, verify session A's expiry does not call onExpire for B

### Step 3: Session cookie helpers

`internal/proxy/session.go` — cookie extraction and construction. The
session store itself (`session.Store`) already exists from phase 0-1.
This step adds the HTTP cookie layer on top.

```go
package proxy

import (
	"net/http"
)

const cookieName = "blockyard_session"

// extractSessionID reads the blockyard_session cookie from the request.
// Returns empty string if the cookie is missing or empty.
func extractSessionID(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return ""
	}
	return c.Value
}

// sessionCookie builds a Set-Cookie header value for the given session
// ID and app name. Path is scoped to /app/{name}/ so the cookie is not
// sent to other apps or the API.
func sessionCookie(sessionID, appName string) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    sessionID,
		Path:     "/app/" + appName + "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}
```

No `Secure` flag in v0 — TLS is terminated externally (Caddy, nginx,
cloud LB). When v1 adds native TLS, the flag is set based on config.

**Tests:**

- `TestExtractSessionID` — request with cookie returns session ID
- `TestExtractSessionIDMissing` — request without cookie returns ""
- `TestExtractSessionIDEmpty` — cookie with empty value returns ""
- `TestSessionCookie` — verify name, path, HttpOnly, SameSite fields

### Step 4: Cold-start holding

`internal/proxy/coldstart.go` — spawns a worker if none exists for the
app, then polls health until ready. Returns the worker ID and address.

```go
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

var (
	errMaxWorkers    = errors.New("max workers reached")
	errNoBundle      = errors.New("app has no active bundle")
	errHealthTimeout = errors.New("worker did not become healthy in time")
)

// ensureWorker returns an existing healthy worker for the app, or spawns
// a new one and waits for it to become healthy.
func ensureWorker(ctx context.Context, srv *server.Server, app *db.AppRow) (workerID, addr string, err error) {
	// 1. Check for existing worker
	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) > 0 {
		wid := workerIDs[0]
		a, ok := srv.Registry.Get(wid)
		if ok {
			return wid, a, nil
		}
		// Registry miss — try to re-resolve address
		a, err := srv.Backend.Addr(ctx, wid)
		if err == nil {
			srv.Registry.Set(wid, a)
			return wid, a, nil
		}
		// Worker unreachable — evict stale entry and spawn fresh
		slog.Warn("evicting stale worker", "worker_id", wid, "error", err)
		srv.Workers.Delete(wid)
		srv.Registry.Delete(wid)
		srv.Sessions.DeleteByWorker(wid)
	}

	// 2. Check global worker limit
	if srv.Workers.Count() >= srv.Config.Proxy.MaxWorkers {
		return "", "", errMaxWorkers
	}

	// 3. Must have an active bundle
	if app.ActiveBundle == nil {
		return "", "", errNoBundle
	}

	// 4. Build WorkerSpec and spawn
	wid := uuid.New().String()
	paths := bundle.NewBundlePaths(
		srv.Config.Storage.BundleServerPath, app.ID, *app.ActiveBundle,
	)

	labels := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    app.ID,
		"dev.blockyard/worker-id": wid,
		"dev.blockyard/role":      "worker",
	}

	spec := backend.WorkerSpec{
		AppID:       app.ID,
		WorkerID:    wid,
		Image:       srv.Config.Docker.Image,
		BundlePath:  paths.Unpacked,
		LibraryPath: paths.Library,
		WorkerMount: srv.Config.Storage.BundleWorkerPath,
		ShinyPort:   srv.Config.Docker.ShinyPort,
		MemoryLimit: ptrOr(app.MemoryLimit, ""),
		CPULimit:    ptrOr(app.CPULimit, 0.0),
		Labels:      labels,
	}

	if err := srv.Backend.Spawn(ctx, spec); err != nil {
		return "", "", fmt.Errorf("spawn worker: %w", err)
	}

	// 5. Resolve address and register
	a, err := srv.Backend.Addr(ctx, wid)
	if err != nil {
		// Spawn succeeded but can't resolve address — stop and bail
		srv.Backend.Stop(ctx, wid)
		return "", "", fmt.Errorf("resolve worker address: %w", err)
	}

	srv.Workers.Set(wid, server.ActiveWorker{AppID: app.ID})
	srv.Registry.Set(wid, a)

	// 6. Cold-start hold — poll health with exponential backoff
	if err := pollHealthy(ctx, srv, wid); err != nil {
		// Health check timed out — evict the worker
		srv.Workers.Delete(wid)
		srv.Registry.Delete(wid)
		srv.Backend.Stop(context.Background(), wid)
		return "", "", err
	}

	slog.Info("worker ready",
		"worker_id", wid, "app_id", app.ID, "addr", a)
	return wid, a, nil
}

// pollHealthy polls backend.HealthCheck with exponential backoff until
// the worker is healthy or worker_start_timeout expires.
func pollHealthy(ctx context.Context, srv *server.Server, workerID string) error {
	timeout := srv.Config.Proxy.WorkerStartTimeout.Duration
	deadline := time.Now().Add(timeout)
	interval := 100 * time.Millisecond
	maxInterval := 2 * time.Second

	for {
		if time.Now().After(deadline) {
			return errHealthTimeout
		}

		if srv.Backend.HealthCheck(ctx, workerID) {
			return nil
		}

		// Exponential backoff capped at maxInterval
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		interval = min(interval*2, maxInterval)
	}
}

func ptrOr[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}
```

**Backoff schedule:** 100ms, 200ms, 400ms, 800ms, 1.6s, 2s, 2s, ...
With a 60s default timeout, that's ~30 health checks before giving up.
Shiny apps typically start in 2–10s.

**Stale worker eviction.** If `ForApp` returns a worker but the registry
miss and `Addr` call both fail, the worker is stale (container crashed
or was removed externally). The function evicts it and proceeds to spawn
a fresh one. Phase 0-6 adds a health poller that catches this sooner,
but the proxy needs to handle it defensively regardless.

**Tests:**

- `TestEnsureWorkerReusesExisting` — pre-register a worker in the
  `WorkerMap` and `Registry`, call `ensureWorker`, verify no spawn
- `TestEnsureWorkerSpawnsNew` — empty worker map, call `ensureWorker`,
  verify `Backend.Spawn` called and worker registered
- `TestEnsureWorkerMaxWorkersRejects` — fill worker map to `MaxWorkers`,
  call `ensureWorker`, verify `errMaxWorkers`
- `TestEnsureWorkerNoBundleRejects` — app with `ActiveBundle == nil`,
  verify `errNoBundle`
- `TestPollHealthySucceeds` — mock returns healthy after 3 checks,
  verify no error
- `TestPollHealthyTimeout` — mock always returns unhealthy, verify
  `errHealthTimeout` after deadline

### Step 5: HTTP forwarding

`internal/proxy/forward.go` — forwards HTTP requests to the worker,
stripping the `/app/{name}` prefix.

```go
package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// forwardHTTP proxies an HTTP request to the worker at addr. The
// /app/{name} prefix is stripped from the path before forwarding.
func forwardHTTP(w http.ResponseWriter, r *http.Request, addr, appName string, transport http.RoundTripper) {
	target := &url.URL{
		Scheme: "http",
		Host:   addr,
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport

	// Rewrite the request: strip prefix, set host, add forwarded headers
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Path = stripAppPrefix(req.URL.Path, appName)
		req.URL.RawPath = ""
		req.Host = addr
	}

	proxy.ServeHTTP(w, r)
}

// stripAppPrefix removes /app/{name} from the start of a URL path.
// Always returns a path starting with /.
func stripAppPrefix(path, appName string) string {
	prefix := "/app/" + appName
	stripped := strings.TrimPrefix(path, prefix)
	if stripped == "" || stripped[0] != '/' {
		return "/" + stripped
	}
	return stripped
}
```

`httputil.ReverseProxy` handles:

- Hop-by-hop header removal (`Connection`, `Keep-Alive`, etc.)
- `X-Forwarded-For` header (appended automatically)
- Response streaming (chunked transfer, SSE, long-poll)
- Connection pooling via the shared `http.Transport`

A single `http.Transport` is created once in the proxy handler setup
and shared across all requests. `httputil.NewSingleHostReverseProxy`
is called per-request (lightweight — it allocates a struct with a few
function pointers) because the target varies per-request.

The `Director` is wrapped rather than replaced: the original director
from `NewSingleHostReverseProxy` sets `req.URL.Scheme`, `req.URL.Host`,
and rewrites headers. Our wrapper then strips the app prefix and
overrides `req.Host`.

**Tests:**

- `TestStripAppPrefix` — table-driven:
  - `/app/myapp/` → `/`
  - `/app/myapp/foo/bar` → `/foo/bar`
  - `/app/myapp` → `/`
  - `/app/myapp/foo?q=1` → `/foo?q=1` (query string preserved by
    the proxy, not by `stripAppPrefix`)

### Step 6: WebSocket forwarding

`internal/proxy/ws.go` — accepts a WebSocket upgrade from the client,
connects to (or reclaims from cache) a backend session, and forwards
messages bidirectionally. Integrates with `wsSession` for reconnect
support with message buffering.

```go
package proxy

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"

	"github.com/cynkra/blockyard/internal/server"
)

// isWebSocketUpgrade checks whether the request is a WebSocket upgrade.
func isWebSocketUpgrade(r *http.Request) bool {
	return r.Header.Get("Upgrade") == "websocket"
}

// shuttleWS accepts a WebSocket from the client, connects to (or
// reclaims from cache) a backend session, and forwards messages
// bidirectionally until one side closes.
//
// The backend→client direction is handled by the wsSession's persistent
// reader goroutine. This function handles the client→backend direction
// in the current goroutine — no additional goroutines are spawned.
//
// If the client disconnects while the backend is still alive, the
// session is cached for possible reconnect within the TTL. Any
// messages the backend sends during the disconnect window are buffered
// and replayed on reconnect.
// If the backend disconnects, the client receives a close frame and
// the session is discarded.
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

	// Get existing session from cache, or create a new one
	session := cache.Take(sessionID)
	if session == nil {
		// No cached session — dial the backend
		backendURL := "ws://" + addr + stripAppPrefix(r.URL.Path, appName)
		backendConn, _, dialErr := websocket.Dial(r.Context(), backendURL, nil)
		if dialErr != nil {
			slog.Warn("ws backend dial failed",
				"addr", addr, "error", dialErr)
			clientConn.Close(websocket.StatusInternalError,
				"backend connect failed")
			return
		}
		session = newSession(backendConn)
	}

	// Attach client — replays buffered messages, then live forwarding
	if err := session.Attach(clientConn); err != nil {
		slog.Warn("ws attach failed", "error", err)
		clientConn.Close(websocket.StatusGoingAway, "backend disconnected")
		return
	}

	// client → backend: read from client, write to backend.
	// This is the only goroutine that reads from clientConn and the
	// only goroutine that writes to backendConn.
	var backendGone bool
	for {
		typ, data, err := clientConn.Read(context.Background())
		if err != nil {
			break
		}
		if err := session.backendConn.Write(context.Background(), typ, data); err != nil {
			backendGone = true
			break
		}
	}

	// Detach client from session — reader goroutine switches to buffering
	session.Detach()
	clientConn.CloseNow()

	// Decide whether to cache or discard the session
	if backendGone || session.IsClosed() {
		// Backend died — no point caching a dead session
		session.Close()
		srv.Sessions.Delete(sessionID)
	} else {
		// Client disconnected, backend still alive — cache for reconnect
		slog.Debug("ws client disconnected, caching session",
			"session_id", sessionID)
		cache.Put(sessionID, session, func() {
			// TTL expired without reconnect — clean up session
			workerID, ok := srv.Sessions.Get(sessionID)
			if !ok {
				return
			}
			srv.Sessions.Delete(sessionID)
			// If no other sessions reference this worker, it's idle.
			// Phase 0-6 adds evict_worker here. For now, the health
			// poller will eventually clean up idle workers.
			if srv.Sessions.CountForWorker(workerID) == 0 {
				slog.Info("ws cache expired, worker has no sessions",
					"worker_id", workerID, "session_id", sessionID)
			}
		})
	}
}
```

**No shared context, no goroutines.** Unlike a naive two-goroutine
shuttle, `shuttleWS` runs the client→backend direction in its own
goroutine (the HTTP handler's) and delegates the backend→client
direction to the `wsSession`'s persistent reader goroutine. This
eliminates the need for context cancellation to coordinate shutdown
and avoids `coder/websocket`'s behavior of closing connections on
context cancellation.

**Backend disconnect detection.** The loop exits on either a client
Read error or a backend Write error. A Write error means the backend
connection is broken. The reader goroutine may also independently
discover the backend is dead (Read error), setting `session.closed`.
Both paths are checked before deciding whether to cache: if
`backendGone || session.IsClosed()`, the session is discarded and the
session mapping is cleaned up immediately.

**Cache-on-client-disconnect.** Shiny uses WebSocket for its reactive
communication. Browser tab switches, mobile lock screens, and brief
network interruptions cause the client WebSocket to close. Without
caching, the entire Shiny session state would be lost — the user sees
a grey screen and must reload. By caching the session for
`ws_cache_ttl` (default 60s), a reconnecting client resumes its Shiny
session seamlessly with buffered messages replayed.

**Cache expiry callback.** When the TTL fires without a reconnect, the
callback cleans up the session mapping. In phase 0-5 it logs a warning;
phase 0-6 wires in `evict_worker` to stop the container if no other
sessions reference it.

**Tests:**

- `TestShuttleWSBidirectional` — mock server echoes messages. Client
  sends "hello", verifies echo received.
- `TestShuttleWSCacheOnClientDisconnect` — client closes, verify
  `cache.Take` returns non-nil session with buffered messages
- `TestShuttleWSBackendDisconnect` — backend closes, verify client
  receives close frame and cache is empty
- `TestShuttleWSReconnectReplaysBuffer` — client disconnects, backend
  sends messages, client reconnects, verify buffered messages received

### Step 7: Proxy handler

`internal/proxy/proxy.go` — the main proxy handler that ties together
session management, cold-start, and forwarding.

```go
package proxy

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/server"
)

// Handler returns an http.Handler that proxies requests to Shiny app
// workers. It manages session cookies, cold-starts workers on demand,
// and forwards HTTP and WebSocket traffic.
//
// The returned handler captures a shared WsCache and http.Transport
// that persist for the server's lifetime.
func Handler(srv *server.Server) http.Handler {
	cache := NewWsCache(srv.Config.Proxy.WsCacheTTL.Duration)
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appName := chi.URLParam(r, "name")

		// 1. Look up app by name
		app, err := srv.DB.GetAppByName(appName)
		if err != nil {
			slog.Error("proxy: db error", "app", appName, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if app == nil {
			http.Error(w, "app not found", http.StatusNotFound)
			return
		}

		// 2. Active bundle guard — defense in depth.
		// The invariant is: if workers exist, the app has an active
		// bundle. This is enforced by stopping workers when
		// active_bundle is cleared (see step 12). This check catches
		// any violation of that invariant rather than silently
		// forwarding to a stale worker.
		if app.ActiveBundle == nil {
			http.Error(w, "app has no active bundle", http.StatusServiceUnavailable)
			return
		}

		// 3. Session resolution
		sessionID := extractSessionID(r)
		var workerID, addr string
		isNewSession := false

		if sessionID != "" {
			// Existing session — look up pinned worker
			wid, ok := srv.Sessions.Get(sessionID)
			if ok {
				a, ok := srv.Registry.Get(wid)
				if ok {
					// Worker alive and addressable
					workerID, addr = wid, a
				}
			}
		}

		if workerID == "" {
			// No valid session or stale worker — cold start
			isNewSession = true
			sessionID = uuid.New().String()

			wid, a, err := ensureWorker(r.Context(), srv, app)
			if err != nil {
				switch err {
				case errMaxWorkers:
					http.Error(w, "server at capacity", http.StatusServiceUnavailable)
				case errNoBundle:
					http.Error(w, "app has no active bundle", http.StatusServiceUnavailable)
				case errHealthTimeout:
					http.Error(w, "app failed to start", http.StatusServiceUnavailable)
				default:
					slog.Error("proxy: ensure worker failed",
						"app", appName, "error", err)
					http.Error(w, "internal error", http.StatusInternalServerError)
				}
				return
			}
			workerID, addr = wid, a
			srv.Sessions.Set(sessionID, workerID)
		}

		// 4. Set cookie on new sessions
		if isNewSession {
			http.SetCookie(w, sessionCookie(sessionID, appName))
		}

		// 5. Dispatch — WebSocket or HTTP
		if isWebSocketUpgrade(r) {
			shuttleWS(w, r, addr, appName, sessionID, cache, srv)
		} else {
			forwardHTTP(w, r, addr, appName, transport)
		}
	})
}
```

**Active bundle guard.** The proxy checks `ActiveBundle != nil` before
session resolution. This catches two cases: (a) the normal cold-start
path where `ensureWorker` would also check, and (b) the reuse path
where an existing worker was spawned from a bundle that has since been
cleared. Case (b) is a bug in the prior reference implementation — a
worker could keep serving after its bundle was removed. The primary fix
is in step 12 (enforce the invariant), but this guard provides
defense in depth.

**Session flow.** The handler checks for an existing session cookie
first. If found and the pinned worker is still alive (in registry), the
request is forwarded immediately with no cold-start delay. If the cookie
is missing, empty, or the pinned worker is gone, a new session is
created, `ensureWorker` is called to spawn or reuse a worker, and a
`Set-Cookie` header is added to the response.

**Worker reuse.** In v0, `ensureWorker` returns the first existing
worker for the app (if any), so sessions typically share a worker. This
is a simplification, not a constraint — multiple workers per app can
exist (e.g., during a redeploy or a cold-start race). The v0 constraint
is **one session per worker**; `max_sessions_per_worker` enforcement is
a v1 concern tied to multi-worker load balancing.

**Error mapping.** Proxy errors map to 503 (capacity, no bundle, start
timeout) or 500 (unexpected). The error responses are plain text, not
JSON — end users see these, not API clients. Phase 0-5 keeps them
simple; a custom error page is a future enhancement.

### Step 8: Trailing-slash redirect

`internal/proxy/proxy.go` — redirect `/app/{name}` (no trailing slash)
to `/app/{name}/`, preserving query string.

```go
// RedirectTrailingSlash redirects /app/{name} to /app/{name}/. Shiny
// apps use relative URLs for assets and WebSocket connections, so the
// trailing slash is required for correct path resolution.
func RedirectTrailingSlash(w http.ResponseWriter, r *http.Request) {
	appName := chi.URLParam(r, "name")
	target := "/app/" + appName + "/"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}
```

The 301 is cached by browsers, so subsequent visits without the
trailing slash skip the round-trip. Shiny's `<base>` tag and relative
asset URLs (`shiny.js`, `shared/`, etc.) depend on the trailing slash
being present — without it, the browser resolves relative paths against
`/app/` instead of `/app/{name}/`.

Query string is preserved — without this, authentication tokens or
other state passed via query params would be lost on redirect.

### Step 9: Mock backend `SetWSHandler`

Update `internal/backend/mock/mock.go` to support configurable
WebSocket handlers. Without this, the mock's httptest workers only
serve `200 OK` for plain HTTP and cannot accept WebSocket upgrades,
making WS integration tests impossible.

The `MockBackend` gains a `handler` field (default: 200 OK) that is
used when spawning new httptest workers. `SetWSHandler` overrides this
for subsequently spawned workers.

```go
// SetWSHandler sets the HTTP handler used by subsequently spawned mock
// workers. The handler should accept WebSocket upgrades. Workers spawned
// before this call are not affected.
func (b *MockBackend) SetWSHandler(h http.Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handler = h
}
```

In `Spawn`, instead of the hardcoded `200 OK` handler:

```go
func (b *MockBackend) Spawn(ctx context.Context, spec backend.WorkerSpec) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	h := b.handler
	if h == nil {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	}

	ts := httptest.NewServer(h)
	b.workers[spec.WorkerID] = &mockWorker{
		id:     spec.WorkerID,
		spec:   spec,
		server: ts,
	}
	return nil
}
```

### Step 10: Router composition

Update `NewRouter` in `internal/api/router.go` to mount the proxy
handler alongside the API routes.

```go
func NewRouter(srv *server.Server) http.Handler {
	r := chi.NewRouter()

	// Unauthenticated
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	// Proxy routes — unauthenticated (end users access these)
	r.Get("/app/{name}", proxy.RedirectTrailingSlash)
	r.Handle("/app/{name}/*", proxy.Handler(srv))

	// Authenticated API
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(BearerAuth(srv))

		r.Post("/apps", CreateApp(srv))
		r.Get("/apps", ListApps(srv))
		r.Get("/apps/{id}", GetApp(srv))
		r.Patch("/apps/{id}", UpdateApp(srv))
		r.Delete("/apps/{id}", DeleteApp(srv))

		r.Post("/apps/{id}/bundles", UploadBundle(srv))
		r.Get("/apps/{id}/bundles", ListBundles(srv))

		r.Post("/apps/{id}/start", StartApp(srv))
		r.Post("/apps/{id}/stop", StopApp(srv))
		r.Get("/apps/{id}/logs", AppLogs(srv))

		r.Get("/tasks/{taskID}", GetTaskStatus(srv))
		r.Get("/tasks/{taskID}/logs", TaskLogs(srv))
	})

	return r
}
```

The proxy routes use `r.Handle` (not `r.HandleFunc` — `proxy.Handler`
returns `http.Handler`, not `http.HandlerFunc`). The catch-all `/*` in
`/app/{name}/*` matches any sub-path and registers for all HTTP methods
(GET, POST, PUT, etc.) — Shiny sends both GET and POST requests during
its lifecycle.

The `r.Get("/app/{name}", ...)` handles the exact match (no trailing
slash) with a redirect. `r.Handle("/app/{name}/*", ...)` handles
everything under `/app/{name}/` including the root with trailing slash
(`/app/{name}/` matches with `*` = empty).

Proxy routes are NOT behind `BearerAuth` — end users access them
without an API token. The session cookie provides routing affinity,
not access control. v1 adds user authentication via OIDC.

### Step 11: New dependency

```
go get github.com/coder/websocket
```

`coder/websocket` (formerly `nhooyr.io/websocket`) is context-aware
and maintained by Coder. It provides both server-side `Accept` and
client-side `Dial`, plus clean `Read`/`Write` that respect context
cancellation. This aligns with the dependency choice in `plan.md`.

### Step 12: Enforce active bundle invariant

**Bug fix (inherited from prior reference implementation).** When
`active_bundle` is cleared — via `PATCH` setting it to null, or via
bundle deletion that removes the active bundle — any running workers
for the app must be stopped. Without this, the proxy's existing-worker
fast path returns a worker that was spawned from a now-removed bundle,
silently serving stale code.

**Invariant: if workers exist for an app, the app has an active
bundle.**

This requires changes in phase 0-4 endpoints:

1. **`PATCH /apps/{id}`** — if the update clears `active_bundle` (sets
   it to null), call `stopAppWorkers` before returning.
2. **`DELETE /apps/{id}`** — already stops workers (phase 0-4). No
   change needed.
3. **Bundle deletion that removes the active bundle** — if a deleted
   bundle was the app's `active_bundle`, clear `active_bundle` and
   stop workers.

The proxy handler's `ActiveBundle != nil` check (step 7) is a
defense-in-depth guard that catches any violation of this invariant.

### Step 13: Integration tests

`internal/proxy/proxy_test.go` — tests that exercise the proxy handler
with a mock backend. The mock backend starts `httptest` servers as fake
workers, so HTTP and WebSocket traffic flows through the full proxy
stack without Docker.

**Test helpers:**

```go
package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/api"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/testutil"
)

func testProxyServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	tmp := t.TempDir()

	cfg := &config.Config{
		Server: config.ServerConfig{Token: "test-token"},
		Docker: config.DockerConfig{
			Image:     "test-image",
			ShinyPort: 3838,
		},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{
			WsCacheTTL:         config.Duration{Duration: 5 * time.Second},
			WorkerStartTimeout: config.Duration{Duration: 5 * time.Second},
			MaxWorkers:         10,
		},
	}

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	handler := api.NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return srv, ts
}

// createAppWithBundle creates an app, uploads a bundle, and waits for
// the mock restore to complete. Does NOT start the app — no worker is
// spawned. Returns the app ID.
func createAppWithBundle(t *testing.T, ts *httptest.Server, name string) string {
	t.Helper()

	// Create app
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"`+name+`"}`)))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	id := created["id"].(string)

	// Upload bundle
	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(req)
	time.Sleep(200 * time.Millisecond) // wait for mock restore

	return id
}

// createAndStartApp creates an app, uploads a bundle, waits for the
// mock restore, and starts the app via the API.
func createAndStartApp(t *testing.T, ts *httptest.Server, name string) {
	t.Helper()
	id := createAppWithBundle(t, ts, name)

	// Start app
	req, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/start", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(req)
}
```

**HTTP tests:**

```go
func TestProxyHTTPForward(t *testing.T) {
	_, ts := testProxyServer(t)
	createAndStartApp(t, ts, "my-app")

	// Hit the proxy
	resp, err := http.Get(ts.URL + "/app/my-app/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestProxySetsSessionCookie(t *testing.T) {
	_, ts := testProxyServer(t)
	createAndStartApp(t, ts, "my-app")

	resp, err := http.Get(ts.URL + "/app/my-app/")
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, c := range resp.Cookies() {
		if c.Name == "blockyard_session" && c.Value != "" {
			found = true
			if c.Path != "/app/my-app/" {
				t.Errorf("expected path /app/my-app/, got %s", c.Path)
			}
			if !c.HttpOnly {
				t.Error("expected HttpOnly cookie")
			}
		}
	}
	if !found {
		t.Error("expected blockyard_session cookie")
	}
}

func TestProxySessionReuse(t *testing.T) {
	srv, ts := testProxyServer(t)
	createAndStartApp(t, ts, "my-app")

	// First request — get session cookie
	resp, _ := http.Get(ts.URL + "/app/my-app/")
	var sessionCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "blockyard_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie")
	}

	initialWorkerCount := srv.Workers.Count()

	// Second request with same cookie — should reuse worker
	req, _ := http.NewRequest("GET", ts.URL+"/app/my-app/", nil)
	req.AddCookie(sessionCookie)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if srv.Workers.Count() != initialWorkerCount {
		t.Errorf("expected %d workers (reuse), got %d",
			initialWorkerCount, srv.Workers.Count())
	}
}

func TestProxyTrailingSlashRedirect(t *testing.T) {
	_, ts := testProxyServer(t)
	createAndStartApp(t, ts, "my-app")

	// Don't follow redirects
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + "/app/my-app")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("expected 301, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/app/my-app/" {
		t.Errorf("expected redirect to /app/my-app/, got %s", loc)
	}
}

func TestProxyTrailingSlashPreservesQuery(t *testing.T) {
	_, ts := testProxyServer(t)
	createAndStartApp(t, ts, "my-app")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + "/app/my-app?token=abc")
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	if loc != "/app/my-app/?token=abc" {
		t.Errorf("expected redirect to /app/my-app/?token=abc, got %s", loc)
	}
}

func TestProxyNonexistentApp(t *testing.T) {
	_, ts := testProxyServer(t)
	resp, err := http.Get(ts.URL + "/app/nonexistent/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestProxyAppWithoutBundleReturns503(t *testing.T) {
	_, ts := testProxyServer(t)
	// Create app but don't upload a bundle
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"no-bundle"}`)))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(req)

	resp, err := http.Get(ts.URL + "/app/no-bundle/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

func TestProxyAtCapacityReturns503(t *testing.T) {
	srv, ts := testProxyServer(t)
	// Create an app with a bundle but don't start it — no worker spawned
	createAppWithBundle(t, ts, "full-app")

	// Fill worker map to max_workers with fake entries
	for i := 0; i < srv.Config.Proxy.MaxWorkers; i++ {
		srv.Workers.Set(
			fmt.Sprintf("fake-%d", i),
			server.ActiveWorker{AppID: "fake"},
		)
	}

	// Hit the proxy — should 503 because no worker exists for this app
	// and we're at capacity so ensureWorker can't spawn one
	resp, err := http.Get(ts.URL + "/app/full-app/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}
```

**WebSocket tests:**

```go
func TestProxyWebSocketEcho(t *testing.T) {
	srv, ts := testProxyServer(t)
	// Configure mock to accept WS and echo messages
	be := srv.Backend.(*mock.MockBackend)
	be.SetWSHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			c.Write(context.Background(), typ, data)
		}
	}))

	createAndStartApp(t, ts, "echo-app")

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/echo-app/"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	// Send a message and verify echo
	if err := conn.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageText || string(data) != "hello" {
		t.Errorf("expected text 'hello', got %v %q", typ, data)
	}
}
```

## New source files

| File | Purpose |
|---|---|
| `internal/proxy/proxy.go` | Proxy handler, trailing-slash redirect, `Handler()` constructor |
| `internal/proxy/forward.go` | HTTP forwarding via `httputil.ReverseProxy`, prefix stripping |
| `internal/proxy/ws.go` | `shuttleWS`, `isWebSocketUpgrade` — WebSocket forwarding with session caching |
| `internal/proxy/wssession.go` | `wsSession` — persistent backend reader goroutine with message buffering |
| `internal/proxy/wscache.go` | `WsCache` — TTL-based cache for `wsSession` entries |
| `internal/proxy/session.go` | Session cookie helpers — `extractSessionID`, `sessionCookie` |
| `internal/proxy/coldstart.go` | `ensureWorker`, `pollHealthy` — cold-start orchestration |
| `internal/proxy/proxy_test.go` | Integration tests — HTTP, WS, session, redirect, error cases |
| `internal/proxy/wssession_test.go` | Unit tests for `wsSession` |
| `internal/proxy/wscache_test.go` | Unit tests for `WsCache` |
| `internal/proxy/session_test.go` | Unit tests for cookie helpers |
| `internal/proxy/coldstart_test.go` | Unit tests for `ensureWorker` and `pollHealthy` |

## Modified files

| File | Change |
|---|---|
| `internal/api/router.go` | Add proxy routes (`/app/{name}`, `/app/{name}/*`) to `NewRouter` |
| `internal/api/apps.go` | Enforce active bundle invariant: stop workers when `active_bundle` is cleared |
| `internal/backend/mock/mock.go` | Add `handler` field and `SetWSHandler` method for WS test support |
| `go.mod` | Add `github.com/coder/websocket` |

## Implementation notes

- **WsCache lives in the proxy package.** It is created once in
  `Handler()` and shared across all proxy requests via closure capture.
  It is not placed on `server.Server` because only the proxy code
  accesses it. The cache expiry callback closes over `*server.Server`
  to access session state.

- **Session cookies are unsigned.** In v0 there is no user
  authentication on the app plane, so the cookie carries routing
  affinity only, not identity. A user who forges a session cookie just
  routes to a different worker — there is no privilege escalation.
  v1 switches to signed cookies when OIDC is added.

- **Worker reuse, not a one-worker-per-app constraint.** `ensureWorker`
  returns the first existing worker for the app rather than spawning a
  new one per session, so sessions typically share a worker. However,
  multiple workers per app can exist (e.g., cold-start race, redeploy
  overlap). The v0 constraint is **one session per worker**.
  `max_sessions_per_worker` enforcement arrives in v1 with multi-worker
  load balancing.

- **Cold-start race.** Two concurrent requests for the same app with
  no existing worker could both enter `ensureWorker` and spawn two
  workers. This is benign in v0 — both workers register, sessions pin
  to whichever finishes first, and the health poller (phase 0-6)
  eventually evicts idle workers. A spawn mutex per app would prevent
  this but adds complexity that isn't justified at v0 scale.

- **No response body on proxy errors.** Proxy error responses are
  plain text (`http.Error`), not JSON. End users see these in the
  browser, not API clients. Keeping them simple is intentional — a
  custom HTML error page is a future enhancement.

- **`httputil.ReverseProxy` per request.** A new `ReverseProxy` struct
  is allocated per request because the target address varies. The
  allocation is cheap (a few pointers), and the underlying
  `http.Transport` is shared for connection pooling.

- **Active bundle invariant.** The system maintains the invariant: if
  workers exist for an app, the app has an active bundle. This is
  enforced by stopping workers when `active_bundle` is cleared (step
  12) and checked defensively in the proxy handler (step 7). This
  fixes a bug present in the prior reference implementation where
  workers could continue serving after their bundle was removed.

- **Phase 0-6 integration points.** Two places in the proxy code
  reference phase 0-6 work: (1) `ensureWorker` should call
  `ops.SpawnLogCapture` after a successful spawn, and (2) the WsCache
  expiry callback should call `ops.EvictWorker` instead of logging.
  Both are stubbed with comments in phase 0-5 and wired in phase 0-6.

## Exit criteria

Phase 0-5 is done when:

- `GET /app/{name}` returns 301 redirect to `/app/{name}/`
- `GET /app/{name}?q=1` redirects to `/app/{name}/?q=1` (query
  preserved)
- `GET /app/{name}/` proxies to a worker container, returns 200
- Session cookie is set on first request, reused on subsequent requests
- Requests with a valid session cookie are forwarded without cold-start
  delay
- Cold-start spawns a worker, polls health, and forwards the request
  once healthy
- Cold-start at global `max_workers` limit returns 503
- Cold-start for app without active bundle returns 503
- Cold-start timeout returns 503 and evicts the failed worker
- Proxy returns 503 for app without active bundle even when workers
  from a previous bundle exist (active bundle guard)
- Request to nonexistent app returns 404
- `/app/{name}/sub/path` is forwarded as `/sub/path` to the worker
- WebSocket upgrade is accepted and messages are forwarded
  bidirectionally
- WebSocket backend connection is cached in a `wsSession` on client
  disconnect
- Messages from backend are buffered while client is disconnected
- Cached session is reused on reconnect within `ws_cache_ttl`, with
  buffered messages replayed
- Buffer overflow (1000 messages) closes the session cleanly
- Cached session is closed and session cleaned up after TTL expiry
- Backend disconnect during active session closes both connections
- Backend disconnect during cache window marks session closed
- Mock backend supports `SetWSHandler` for WS integration tests
- Proxy routes are unauthenticated (no bearer token required)
- API routes still require bearer auth (no regression)
- Clearing `active_bundle` stops all running workers for the app
- All existing phase 0-3 and 0-4 tests still pass
- All new unit and integration tests pass
- `go vet ./...` clean
- `go test ./...` green
