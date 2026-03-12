# Phase 0-5: HTTP/WebSocket Reverse Proxy

Route user traffic to Shiny app containers. This is the layer that makes
deployed apps accessible — without it, the server can manage containers
but nobody can use them. Covers session routing, HTTP/WS forwarding,
cold-start holding, and WebSocket session caching.

## Deliverables

1. `WsCache` — WebSocket connection cache for client reconnects
2. Session cookie helpers — set and read `blockyard_session` cookie
3. Cold-start holding — hold initial request while worker starts, poll
   health until ready or `worker_start_timeout` expires
4. HTTP reverse proxy — forward requests to the correct worker
5. WebSocket reverse proxy — upgrade handling and bidirectional forwarding
6. Proxy handler — catch-all for `/app/{name}/` routes, session
   management, cold-start orchestration
7. Trailing-slash redirect — `/app/{name}` → `/app/{name}/`
8. Path prefix stripping — remove `/app/{name}` before forwarding
9. Router composition — proxy routes alongside API routes in `NewRouter`
10. Integration tests — end-to-end HTTP and WebSocket proxying through
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

### Step 1: WsCache

`internal/proxy/wscache.go` — holds backend WebSocket connections after
client disconnect, keyed by session ID. When a client reconnects within
`ws_cache_ttl`, the cached backend connection is reused instead of
opening a new one.

```go
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
```

`Cache` is called when the client end of a WebSocket closes but the
backend connection is still alive. `Take` is called on reconnect — if
the client opens a new WebSocket to the same app within the TTL, the
proxy reuses the backend connection instead of establishing a new one.

The `onExpire` callback is called when the TTL fires without a
reconnect. The proxy uses this to clean up the session mapping and
(once phase 0-6 lands `evict_worker`) evict the worker if no other
sessions reference it.

`time.AfterFunc` runs the callback in its own goroutine, so the lock
inside the callback does not deadlock with `Cache` or `Take`.

**Tests:**

- `TestCacheTakeBeforeExpiry` — cache a connection, take it back,
  verify non-nil and timer stopped
- `TestCacheExpiry` — cache with short TTL (10ms), sleep, verify
  `Take` returns nil and `onExpire` was called
- `TestCacheEvictsExisting` — cache twice for same session, verify
  first connection is closed
- `TestTakeNonexistent` — take from empty cache, verify nil

### Step 2: Session cookie helpers

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

### Step 3: Cold-start holding

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

### Step 4: HTTP forwarding

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

### Step 5: WebSocket forwarding

`internal/proxy/ws.go` — accepts a WebSocket upgrade from the client,
connects to the backend worker, and shuttles messages bidirectionally.
Integrates with `WsCache` for reconnect support.

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
				// If no other sessions reference this worker, it's idle.
				// Phase 0-6 adds evict_worker here. For now, the health
				// poller will eventually clean up idle workers.
				if srv.Sessions.CountForWorker(workerID) == 0 {
					slog.Info("ws cache expired, worker has no sessions",
						"worker_id", workerID, "session_id", sessionID)
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
```

**Bidirectional forwarding model.** Two goroutines shuttle messages in
opposite directions. Each sends its side identifier to the `done`
channel when it finishes. The first value received tells us which side
disconnected first. The `cancel()` call propagates to the other
goroutine via `coder/websocket`'s context-aware `Read` and `Write`.

**Cache-on-client-disconnect.** Shiny uses WebSocket for its reactive
communication. Browser tab switches, mobile lock screens, and brief
network interruptions cause the client WebSocket to close. Without
caching, the entire Shiny session state would be lost — the user sees
a grey screen and must reload. By caching the backend connection for
`ws_cache_ttl` (default 60s), a reconnecting client resumes its Shiny
session seamlessly.

**Cache expiry callback.** When the TTL fires without a reconnect, the
callback cleans up the session mapping. In phase 0-5 it logs a warning;
phase 0-6 wires in `evict_worker` to stop the container if no other
sessions reference it.

**Tests:**

- `TestShuttleWSBidirectional` — mock server echoes messages. Client
  sends "hello", verifies echo received.
- `TestShuttleWSCacheOnClientDisconnect` — client closes, verify
  `cache.Take` returns non-nil backend connection
- `TestShuttleWSBackendDisconnect` — backend closes, verify client
  receives close frame and cache is empty
- `TestCopyWS` — verify messages are forwarded correctly

### Step 6: Proxy handler

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
	cache := NewWsCache()
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

		// 2. Session resolution
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

		// 3. Set cookie on new sessions
		if isNewSession {
			http.SetCookie(w, sessionCookie(sessionID, appName))
		}

		// 4. Dispatch — WebSocket or HTTP
		if isWebSocketUpgrade(r) {
			shuttleWS(w, r, addr, appName, sessionID, cache, srv)
		} else {
			forwardHTTP(w, r, addr, appName, transport)
		}
	})
}
```

**Session flow.** The handler checks for an existing session cookie
first. If found and the pinned worker is still alive (in registry), the
request is forwarded immediately with no cold-start delay. If the cookie
is missing, empty, or the pinned worker is gone, a new session is
created, `ensureWorker` is called to spawn or reuse a worker, and a
`Set-Cookie` header is added to the response.

**Worker reuse.** In v0, `ensureWorker` returns the first existing
worker for the app (if any). All sessions for the same app share one
worker. `max_sessions_per_worker` enforcement is a v1 concern tied to
multi-worker load balancing.

**Error mapping.** Proxy errors map to 503 (capacity, no bundle, start
timeout) or 500 (unexpected). The error responses are plain text, not
JSON — end users see these, not API clients. Phase 0-5 keeps them
simple; a custom error page is a future enhancement.

### Step 7: Trailing-slash redirect

`internal/proxy/proxy.go` — redirect `/app/{name}` (no trailing slash)
to `/app/{name}/`.

```go
// RedirectTrailingSlash redirects /app/{name} to /app/{name}/. Shiny
// apps use relative URLs for assets and WebSocket connections, so the
// trailing slash is required for correct path resolution.
func RedirectTrailingSlash(w http.ResponseWriter, r *http.Request) {
	appName := chi.URLParam(r, "name")
	http.Redirect(w, r, "/app/"+appName+"/", http.StatusMovedPermanently)
}
```

The 301 is cached by browsers, so subsequent visits without the
trailing slash skip the round-trip. Shiny's `<base>` tag and relative
asset URLs (`shiny.js`, `shared/`, etc.) depend on the trailing slash
being present — without it, the browser resolves relative paths against
`/app/` instead of `/app/{name}/`.

### Step 8: Router composition

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
	r.HandleFunc("/app/{name}/*", proxy.Handler(srv))

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

The proxy routes use `HandleFunc` which registers for all HTTP methods
(GET, POST, PUT, etc.) — Shiny sends both GET and POST requests during
its lifecycle. The catch-all `/*` in `/app/{name}/*` matches any
sub-path.

The `r.Get("/app/{name}", ...)` handles the exact match (no trailing
slash) with a redirect. `r.HandleFunc("/app/{name}/*", ...)` handles
everything under `/app/{name}/` including the root with trailing slash
(`/app/{name}/` matches with `*` = empty).

Proxy routes are NOT behind `BearerAuth` — end users access them
without an API token. The session cookie provides routing affinity,
not access control. v1 adds user authentication via OIDC.

### Step 9: New dependency

```
go get github.com/coder/websocket
```

`coder/websocket` (formerly `nhooyr.io/websocket`) is context-aware
and maintained by Coder. It provides both server-side `Accept` and
client-side `Dial`, plus clean `Read`/`Write` that respect context
cancellation. This aligns with the dependency choice in `plan.md`.

### Step 10: Integration tests

`internal/proxy/proxy_test.go` — tests that exercise the proxy handler
with a mock backend. The mock backend starts `httptest` servers as fake
workers, so HTTP and WebSocket traffic flows through the full proxy
stack without Docker.

**Test helper:**

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

// createAndStartApp creates an app, uploads a bundle, waits for the
// mock restore, and starts the app via the API. Returns the app name.
func createAndStartApp(t *testing.T, ts *httptest.Server, name string) {
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

	// Start app
	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/start", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(req)
}
```

**Tests:**

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
	createAndStartApp(t, ts, "app-a")

	// Fill remaining capacity by registering fake workers
	for i := srv.Workers.Count(); i < srv.Config.Proxy.MaxWorkers; i++ {
		srv.Workers.Set(
			fmt.Sprintf("fake-%d", i),
			server.ActiveWorker{AppID: "fake"},
		)
	}

	// Create another app with a bundle
	createAndStartApp(t, ts, "app-b")
	// Note: createAndStartApp uses the API start endpoint which checks
	// the limit. We need to test the proxy path instead.
	// Stop app-b so we can test the proxy cold start at capacity.
	// ... (use API stop, then hit the proxy)
}
```

**WebSocket tests:**

```go
func TestProxyWebSocket(t *testing.T) {
	_, ts := testProxyServer(t)
	createAndStartApp(t, ts, "my-app")

	// Connect via WebSocket
	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/my-app/"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	// The mock worker's httptest server doesn't speak WS by default.
	// This test validates that the proxy correctly upgrades and
	// attempts the backend connection. A more complete test requires
	// configuring the mock to accept WS connections.
}
```

WebSocket integration tests require the mock backend's `httptest`
servers to handle WS upgrades. The mock backend can be extended to
register a WS echo handler on each worker. Full WS round-trip tests:

```go
func TestProxyWebSocketEcho(t *testing.T) {
	srv, ts := testProxyServer(t)
	// Configure mock to accept WS and echo messages
	mock := srv.Backend.(*mock.MockBackend)
	mock.SetWSHandler(func(w http.ResponseWriter, r *http.Request) {
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
	})

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

**Mock backend extension.** `MockBackend` needs a `SetWSHandler` method
that registers a WebSocket handler on each mock worker's `httptest`
server. The default handler serves plain HTTP (200 OK). When
`SetWSHandler` is called, new workers use the provided handler instead.

## New source files

| File | Purpose |
|---|---|
| `internal/proxy/proxy.go` | Proxy handler, trailing-slash redirect, `Handler()` constructor |
| `internal/proxy/forward.go` | HTTP forwarding via `httputil.ReverseProxy`, prefix stripping |
| `internal/proxy/ws.go` | WebSocket forwarding, bidirectional message shuttling, WS upgrade detection |
| `internal/proxy/wscache.go` | `WsCache` — TTL-based backend WebSocket connection cache |
| `internal/proxy/session.go` | Session cookie helpers — `extractSessionID`, `sessionCookie` |
| `internal/proxy/coldstart.go` | `ensureWorker`, `pollHealthy` — cold-start orchestration |
| `internal/proxy/proxy_test.go` | Integration tests — HTTP, WS, session, redirect, error cases |
| `internal/proxy/wscache_test.go` | Unit tests for `WsCache` |
| `internal/proxy/session_test.go` | Unit tests for cookie helpers |
| `internal/proxy/coldstart_test.go` | Unit tests for `ensureWorker` and `pollHealthy` |

## Modified files

| File | Change |
|---|---|
| `internal/api/router.go` | Add proxy routes (`/app/{name}`, `/app/{name}/*`) to `NewRouter` |
| `internal/backend/mock/mock.go` | Add `SetWSHandler` method for WS test support |
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

- **One worker per app in v0.** `ensureWorker` returns the first
  existing worker for the app rather than spawning a new one per
  session. Multiple sessions share the same worker. Per-session workers
  and `max_sessions_per_worker` enforcement arrive in v1 with
  multi-worker load balancing.

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

- **Phase 0-6 integration points.** Two places in the proxy code
  reference phase 0-6 work: (1) `ensureWorker` should call
  `ops.SpawnLogCapture` after a successful spawn, and (2) the WsCache
  expiry callback should call `ops.EvictWorker` instead of logging.
  Both are stubbed with comments in phase 0-5 and wired in phase 0-6.

## Exit criteria

Phase 0-5 is done when:

- `GET /app/{name}` returns 301 redirect to `/app/{name}/`
- `GET /app/{name}/` proxies to a worker container, returns 200
- Session cookie is set on first request, reused on subsequent requests
- Requests with a valid session cookie are forwarded without cold-start
  delay
- Cold-start spawns a worker, polls health, and forwards the request
  once healthy
- Cold-start at global `max_workers` limit returns 503
- Cold-start for app without active bundle returns 503
- Cold-start timeout returns 503 and evicts the failed worker
- Request to nonexistent app returns 404
- `/app/{name}/sub/path` is forwarded as `/sub/path` to the worker
- WebSocket upgrade is accepted and messages are forwarded
  bidirectionally
- WebSocket backend connection is cached on client disconnect
- Cached WebSocket is reused on reconnect within `ws_cache_ttl`
- Cached WebSocket is closed and session cleaned up after TTL expiry
- Proxy routes are unauthenticated (no bearer token required)
- API routes still require bearer auth (no regression)
- All existing phase 0-3 and 0-4 tests still pass
- All new unit and integration tests pass
- `go vet ./...` clean
- `go test ./...` green
