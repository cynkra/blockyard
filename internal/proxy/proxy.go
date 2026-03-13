package proxy

import (
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"strings"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/authz"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
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

		// 1. Look up app by ID (UUID) first, then by name.
		// UUID lookup gives stable URLs that survive app renames.
		app, err := srv.DB.GetApp(appName)
		if err != nil {
			slog.Error("proxy: db error", "app", appName, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if app == nil {
			app, err = srv.DB.GetAppByName(appName)
			if err != nil {
				slog.Error("proxy: db error", "app", appName, "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if app == nil {
			http.Error(w, "app not found", http.StatusNotFound)
			return
		}

		// 1b. ACL check — when OIDC is configured, enforce access control.
		if srv.Config.OIDC != nil {
			caller := auth.CallerFromContext(r.Context())

			rows, dbErr := srv.DB.ListAppAccess(app.ID)
			if dbErr != nil {
				rows = nil // treat as no grants (fail closed)
			}

			grants := make([]authz.AccessGrant, len(rows))
			for i, row := range rows {
				role, _ := authz.ParseContentRole(row.Role)
				grants[i] = authz.AccessGrant{
					AppID:     row.AppID,
					Principal: row.Principal,
					Kind:      authz.AccessKind(row.Kind),
					Role:      role,
					GrantedBy: row.GrantedBy,
					GrantedAt: row.GrantedAt,
				}
			}

			relation := authz.EvaluateAccess(caller, app.Owner, grants, app.AccessType)
			if !relation.CanAccessProxy() {
				if caller == nil {
					http.Redirect(w, r, "/login?return_url="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
					return
				}
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
		}

		// Resolve caller identity (may be nil when OIDC is not configured).
		caller := auth.CallerFromContext(r.Context())
		callerSub := ""
		if caller != nil {
			callerSub = caller.Sub
		}

		// 2. Session resolution
		sessionID := extractSessionID(r)
		var workerID, addr string
		isNewSession := false

		if sessionID != "" {
			// Existing session — look up pinned worker and verify identity.
			entry, ok := srv.Sessions.Get(sessionID)
			if ok {
				// When OIDC is active, reject sessions owned by a different user.
				if entry.UserSub != "" && callerSub != entry.UserSub {
					slog.Warn("proxy: session owner mismatch",
						"session_id", sessionID,
						"session_owner", entry.UserSub,
						"caller", callerSub)
					// Fall through to create a new session for this user.
					ok = false
				}
			}
			if ok {
				a, addrOk := srv.Registry.Get(entry.WorkerID)
				if addrOk {
					workerID, addr = entry.WorkerID, a
					srv.Sessions.Touch(sessionID)
				}
			}
		}

		if workerID == "" {
			// No valid session or stale worker — assign via load balancer
			isNewSession = true
			sessionID = uuid.New().String()

			wid, a, err := ensureWorker(r.Context(), srv, app)
			if err != nil {
				switch err {
				case errMaxWorkers:
					http.Error(w, "server at capacity", http.StatusServiceUnavailable)
				case errCapacityExhausted:
					http.Error(w, "app at capacity", http.StatusServiceUnavailable)
				case errAppDraining:
					http.Error(w, "app is shutting down", http.StatusServiceUnavailable)
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
			srv.Sessions.Set(sessionID, session.Entry{
				WorkerID:   workerID,
				UserSub:    callerSub,
				LastAccess: time.Now(),
			})
		}

		// 3. Set cookie on new sessions
		if isNewSession {
			http.SetCookie(w, sessionCookie(sessionID, appName, r))
		}

		// 4. Inject identity headers when caller is authenticated.
		// Shiny apps read X-Shiny-User and X-Shiny-Groups to identify
		// the logged-in user. Always strip first to prevent spoofing by
		// unauthenticated clients, then re-add from verified identity.
		r.Header.Del("X-Shiny-User")
		r.Header.Del("X-Shiny-Groups")
		if caller != nil {
			r.Header.Set("X-Shiny-User", caller.Sub)
			if len(caller.Groups) > 0 {
				r.Header.Set("X-Shiny-Groups", strings.Join(caller.Groups, ","))
			}
		}

			// 4b. Inject credentials when configured.
		// Single-tenant: injects raw vault token (X-Blockyard-Vault-Token).
		// Shared: injects session reference token (X-Blockyard-Session-Token)
		// that the app exchanges for real credentials via the exchange API.
		injectCredentials(r, srv, app.ID, workerID, app.MaxSessionsPerWorker)

		// 5. Dispatch — WebSocket or HTTP
		if isWebSocketUpgrade(r) {
			shuttleWS(w, r, addr, appName, sessionID, cache, srv)
		} else {
			forwardHTTP(w, r, addr, appName, transport)
		}
	})
}

// injectCredentials handles per-request credential injection.
// For single-tenant containers: injects raw vault token (backwards compat).
// For shared containers: injects a signed session reference token that
// the app exchanges for vault credentials via the credential exchange API.
func injectCredentials(r *http.Request, srv *server.Server, appID, workerID string, maxSessionsPerWorker int) {
	r.Header.Del("X-Blockyard-Vault-Token")
	r.Header.Del("X-Blockyard-Session-Token")

	if srv.VaultClient == nil {
		return
	}

	user := auth.UserFromContext(r.Context())
	if user == nil || user.AccessToken == "" {
		return
	}

	if maxSessionsPerWorker > 1 {
		// Shared container — inject session reference token.
		// The app exchanges this for real credentials.
		now := time.Now().Unix()
		claims := &auth.SessionTokenClaims{
			Sub: user.Sub,
			App: appID,
			Wid: workerID,
			Iat: now,
			Exp: now + int64(auth.SessionTokenTTL.Seconds()),
		}
		token, err := auth.EncodeSessionToken(claims, srv.SessionTokenKey)
		if err != nil {
			slog.Warn("failed to encode session token",
				"sub", user.Sub, "error", err)
			return
		}
		r.Header.Set("X-Blockyard-Session-Token", token)
		return
	}

	// Single-tenant container — inject raw vault token (backwards compat).
	token, ok := srv.VaultTokenCache.Get(user.Sub)
	if !ok {
		var err error
		var ttl time.Duration
		token, ttl, err = srv.VaultClient.JWTLogin(
			r.Context(),
			srv.Config.Openbao.JWTAuthPath,
			user.AccessToken,
		)
		if err != nil {
			slog.Warn("vault JWT login failed", "sub", user.Sub, "error", err)
			return
		}
		if ttl == 0 {
			ttl = srv.Config.Openbao.TokenTTL.Duration
		}
		srv.VaultTokenCache.Set(user.Sub, token, ttl)
	}
	r.Header.Set("X-Blockyard-Vault-Token", token)
}

// RedirectTrailingSlash redirects /app/{name} to /app/{name}/. Shiny
// apps use relative URLs for assets and WebSocket connections, so the
// trailing slash is required for correct path resolution.
func RedirectTrailingSlash(w http.ResponseWriter, r *http.Request) {
	appName := chi.URLParam(r, "name")
	http.Redirect(w, r, "/app/"+appName+"/", http.StatusMovedPermanently)
}
