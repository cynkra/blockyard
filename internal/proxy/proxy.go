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

// RedirectTrailingSlash redirects /app/{name} to /app/{name}/. Shiny
// apps use relative URLs for assets and WebSocket connections, so the
// trailing slash is required for correct path resolution.
func RedirectTrailingSlash(w http.ResponseWriter, r *http.Request) {
	appName := chi.URLParam(r, "name")
	http.Redirect(w, r, "/app/"+appName+"/", http.StatusMovedPermanently)
}
