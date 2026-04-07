package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/authz"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
	"github.com/cynkra/blockyard/internal/telemetry"
)

// Handler returns an http.Handler that proxies requests to Shiny app
// workers. It manages session cookies, cold-starts workers on demand,
// and forwards HTTP and WebSocket traffic.
//
// The returned handler captures a shared WsCache and http.Transport
// that persist for the server's lifetime.
func Handler(srv *server.Server) http.Handler {
	cache := NewWsCache()

	// Limit concurrent WebSocket connections to prevent resource exhaustion.
	// Aligned with max_workers (default 100): each session holds at most
	// one WebSocket, so this bounds memory even if every session is active.
	const maxWSConns = 100
	wsSem := make(chan struct{}, maxWSConns)

	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		MaxConnsPerHost:       25,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		telemetry.ProxyRequests.Inc()
		appName := chi.URLParam(r, "name")

		// 1. Look up app by ID (UUID) first, then by name.
		// UUID lookup gives stable URLs that survive app renames.
		app, err := srv.DB.GetApp(appName)
		if err != nil {
			slog.Error("proxy: db error", "app", appName, "error", err) //nolint:gosec // G706: slog structured logging handles this
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if app == nil {
			app, err = srv.DB.GetAppByName(appName)
			if err != nil {
				slog.Error("proxy: db error", "app", appName, "error", err) //nolint:gosec // G706: slog structured logging handles this
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		// 2. Try alias table (rename fallback).
		if app == nil {
			var phase string
			app, phase, err = srv.DB.GetAppByAlias(appName)
			if err != nil {
				slog.Error("proxy: db error", "app", appName, "error", err) //nolint:gosec // G706: slog structured logging handles this
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if app != nil && phase == "redirect" {
				newPath := "/app/" + app.Name + "/"
				if rest := chi.URLParam(r, "*"); rest != "" {
					newPath += rest
				}
				http.Redirect(w, r, newPath, http.StatusMovedPermanently)
				return
			}
		}
		if app == nil {
			http.Error(w, "app not found", http.StatusNotFound)
			return
		}

		// Reject requests to disabled apps before session routing.
		if !app.Enabled {
			http.Error(w, "app is disabled", http.StatusServiceUnavailable)
			return
		}

		// Resolve caller identity (may be nil when OIDC is not configured).
		caller := auth.CallerFromContext(r.Context())
		callerSub := ""
		if caller != nil {
			callerSub = caller.Sub
		}

		// 1b. ACL check — when OIDC is configured, enforce access control.
		// Also compute the effective access relation for X-Shiny-Access.
		var relation authz.AppRelation
		if srv.Config.OIDC != nil {
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

			relation = authz.EvaluateAccess(caller, app.Owner, grants, app.AccessType)
			if !relation.CanAccessProxy() {
				if caller == nil {
					http.Redirect(w, r, "/login?return_url="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
					return
				}
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
		}

		// Intercept /__blockyard/ internal endpoints before session routing.
		if handleBlockyardInternal(w, r, app, appName, srv) {
			return
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
					slog.Warn("proxy: session owner mismatch", //nolint:gosec // G706: slog structured logging handles this
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
					slog.Debug("proxy: reusing session", //nolint:gosec // G706: slog structured logging handles this
						"app", appName, "session_id", sessionID,
						"worker_id", workerID)
				} else {
					slog.Debug("proxy: session worker not in registry", //nolint:gosec // G706: slog structured logging handles this
						"app", appName, "session_id", sessionID,
						"worker_id", entry.WorkerID)
				}
			}
		}

		if workerID == "" {
			// No valid session or stale worker — assign via load balancer
			isNewSession = true
			sessionID = uuid.New().String()
			slog.Debug("proxy: creating new session", //nolint:gosec // G706: slog structured logging handles this
				"app", appName, "session_id", sessionID)

			// Check if any workers exist before calling ensureWorker.
			// If none exist and this is a browser request, serve the loading
			// page instead of blocking.
			if !hasAvailableWorker(srv, app.ID) && isBrowserRequest(r) {
				go triggerSpawn(srv, app) //nolint:gosec // G118: intentional background spawn, outlives request
				serveLoadingPage(w, app, appName, srv)
				return
			}

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
					slog.Error("proxy: ensure worker failed", //nolint:gosec // G706: slog structured logging handles this
						"app", appName, "error", err)
					http.Error(w, "internal error", http.StatusInternalServerError)
				}
				return
			}
			workerID, addr = wid, a
			wasIdle := srv.Workers.ClearIdleSince(workerID)
			srv.Sessions.Set(sessionID, session.Entry{
				WorkerID:   workerID,
				UserSub:    callerSub,
				LastAccess: time.Now(),
			})
			telemetry.SessionsActive.Inc()

			// Track session in the database for activity metrics.
			if err := srv.DB.CreateSession(sessionID, app.ID, workerID, callerSub); err != nil {
				slog.Warn("proxy: failed to create session record",
					"session_id", sessionID, "error", err)
			}

			// Trigger pre-warm replacement if we just claimed a warm worker.
			if wasIdle && app.PreWarmedSessions > 0 {
				go ensurePreWarmed(context.Background(), srv, app) //nolint:gosec // G118: intentional background pre-warm, outlives request
			}
		}

		// 3. Set cookie on new sessions
		if isNewSession {
			http.SetCookie(w, sessionCookie(sessionID, appName, srv.Config.Server.ExternalURL, srv.Config.Proxy.SessionIdleTTL.Duration))
		}

		// 4. Inject identity headers when caller is authenticated.
		// Shiny apps read X-Shiny-User and X-Shiny-Access to identify
		// the logged-in user and their effective access level. Always
		// strip first to prevent spoofing by unauthenticated clients,
		// then re-add from verified identity.
		r.Header.Del("X-Shiny-User")
		r.Header.Del("X-Shiny-Access")
		r.Header.Del("X-Shiny-Groups")
		// Strip client-supplied forwarding headers so httputil.ReverseProxy
		// builds a fresh X-Forwarded-For from the resolved RemoteAddr.
		r.Header.Del("X-Forwarded-For")
		r.Header.Del("X-Real-IP")
		if caller != nil {
			r.Header.Set("X-Shiny-User", caller.DisplayName())
		}
		r.Header.Set("X-Shiny-Access", relationToAccessLevel(relation))

			// 4b. Inject credentials when configured.
		// Single-tenant: injects raw vault token (X-Blockyard-Vault-Token).
		// Shared: injects session reference token (X-Blockyard-Session-Token)
		// that the app exchanges for real credentials via the exchange API.
		injectCredentials(r, srv, app.ID, workerID, app.MaxSessionsPerWorker)

		// 5. Dispatch — WebSocket or HTTP
		forwardStart := time.Now()
		if isWebSocketUpgrade(r) {
			select {
			case wsSem <- struct{}{}:
				defer func() { <-wsSem }()
			default:
				http.Error(w, "too many WebSocket connections", http.StatusServiceUnavailable)
				return
			}
			shuttleWS(w, r, addr, appName, sessionID, cache, srv)
		} else {
			forwardHTTP(w, r, addr, appName, srv.Config.Server.ExternalURL, transport, srv.Config.Proxy.HTTPForwardTimeout.Duration)
		}
		telemetry.ProxyRequestDuration.Observe(time.Since(forwardStart).Seconds())
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
	if ok {
		slog.Debug("vault token cache hit", "sub", user.Sub)
	} else {
		slog.Debug("vault token cache miss, performing JWT login", "sub", user.Sub)
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
			slog.Debug("vault token TTL is zero, using configured default",
				"sub", user.Sub, "default_ttl", srv.Config.Openbao.TokenTTL.Duration)
			ttl = srv.Config.Openbao.TokenTTL.Duration
		}
		srv.VaultTokenCache.Set(user.Sub, token, ttl)
	}
	r.Header.Set("X-Blockyard-Vault-Token", token)
}

// relationToAccessLevel converts an AppRelation to the X-Shiny-Access
// header value. See the wrap-up design doc for the value table.
func relationToAccessLevel(r authz.AppRelation) string {
	switch r {
	case authz.RelationAdmin, authz.RelationOwner:
		return "owner"
	case authz.RelationContentCollaborator:
		return "collaborator"
	case authz.RelationContentViewer:
		return "viewer"
	case authz.RelationAnonymous:
		return "anonymous"
	default:
		return "anonymous"
	}
}

// RedirectTrailingSlash redirects /app/{name} to /app/{name}/. Shiny
// apps use relative URLs for assets and WebSocket connections, so the
// trailing slash is required for correct path resolution.
func RedirectTrailingSlash(w http.ResponseWriter, r *http.Request) {
	appName := chi.URLParam(r, "name")
	if !isValidAppNameOrID(appName) {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/app/"+appName+"/", http.StatusMovedPermanently)
}

// isValidAppNameOrID checks that a string is safe for use in a redirect URL.
// Accepts app names (lowercase + digits + hyphens) and UUIDs.
func isValidAppNameOrID(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}
