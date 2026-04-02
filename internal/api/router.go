package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	httpSwagger "github.com/swaggo/http-swagger/v2"
	"github.com/swaggo/swag/v2"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/docs"
	"github.com/cynkra/blockyard/internal/proxy"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/telemetry"
	"github.com/cynkra/blockyard/internal/ui"
)

// maxJSONBodySize limits the request body for JSON API endpoints to 1 MiB.
// Bundle uploads have their own limit via http.MaxBytesReader.
const maxJSONBodySize = 1 << 20

// responseCapture wraps http.ResponseWriter to capture the status code.
type responseCapture struct {
	http.ResponseWriter
	status int
}

func (rc *responseCapture) WriteHeader(code int) {
	rc.status = code
	rc.ResponseWriter.WriteHeader(code)
}

func (rc *responseCapture) Unwrap() http.ResponseWriter {
	return rc.ResponseWriter
}

// requestLogger logs each HTTP request with method, path, status, and duration.
// Health/readiness probes are logged at Debug to reduce noise.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rc := &responseCapture{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rc, r)
		duration := time.Since(start)

		path := r.URL.Path
		status := rc.status

		// Health/readiness probes at Debug to avoid log spam.
		if path == "/healthz" || path == "/readyz" {
			slog.Debug("request", //nolint:gosec // G706: slog structured logging handles this
				"method", r.Method,
				"path", path,
				"status", status,
				"duration_ms", duration.Milliseconds())
			return
		}

		attrs := []any{
			"method", r.Method,
			"path", path,
			"status", status,
			"duration_ms", duration.Milliseconds(),
			"remote", r.RemoteAddr,
		}

		switch {
		case status >= 500:
			slog.Error("request", attrs...) //nolint:gosec // G706: slog structured logging handles this
		case status >= 400:
			slog.Warn("request", attrs...) //nolint:gosec // G706: slog structured logging handles this
		default:
			slog.Info("request", attrs...) //nolint:gosec // G706: slog structured logging handles this
		}
	})
}

// limitBody is a middleware that caps request body size to prevent
// memory exhaustion from oversized JSON payloads.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
		next.ServeHTTP(w, r)
	})
}

// securityHeaders is a middleware that sets common security response headers.
// CSP is intentionally not set here because proxied Shiny apps serve
// inline scripts/styles that a strict policy would break. API-only CSP
// is applied separately via apiCSP.
func securityHeaders(externalURL string) func(http.Handler) http.Handler {
	isHTTPS := strings.HasPrefix(externalURL, "https://")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			if isHTTPS {
				w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// versionHeader sets the X-Blockyard-Version response header so that
// clients can detect incompatible server versions.
func versionHeader(version string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if version != "" {
				w.Header().Set("X-Blockyard-Version", version)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// apiCSP sets a strict Content-Security-Policy for JSON API endpoints
// where no HTML rendering occurs, and prevents caching of sensitive
// API responses by intermediate proxies and browsers.
func apiCSP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// swaggerDocJSON serves the OpenAPI spec from the swag/v2 registry.
func swaggerDocJSON(w http.ResponseWriter, _ *http.Request) {
	doc, err := swag.ReadDoc()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write([]byte(doc))
}

func NewRouter(srv *server.Server, startBG func()) http.Handler {
	r := chi.NewRouter()

	// Request logging (outermost to capture status/duration for all routes).
	r.Use(requestLogger)

	// Resolve real client IP when behind a trusted reverse proxy.
	// Must run before rate limiting so limits are per-client, not per-proxy.
	r.Use(realIPMiddleware(srv.Config.Server.TrustedProxies))

	// Global security headers (HSTS when HTTPS).
	r.Use(securityHeaders(srv.Config.Server.ExternalURL))

	// Advertise server version to clients.
	r.Use(versionHeader(srv.Version))

	// OpenTelemetry tracing middleware (only when configured).
	if srv.Config.Telemetry != nil && srv.Config.Telemetry.OTLPEndpoint != "" {
		r.Use(telemetry.TracingMiddleware())
	}

	authDeps := srv.AuthDeps()

	// Operational endpoints: when a management listener is configured,
	// these move there. Otherwise serve them on the main listener.
	if srv.Config.Server.ManagementBind == "" {
		r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			if srv.Draining.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte("draining"))
				return
			}
			w.Write([]byte("ok"))
		})
		r.Get("/readyz", readyzHandler(srv, false))

		// Prometheus metrics endpoint (requires API auth on main listener).
		if srv.Config.Telemetry != nil && srv.Config.Telemetry.MetricsEnabled {
			r.Group(func(r chi.Router) {
				r.Use(APIAuth(srv))
				r.Handle("/metrics", promhttp.Handler())
			})
		}
	}

	// UI routes — soft auth populates session context if available.
	uiHandler := ui.New()
	r.Group(func(r chi.Router) {
		r.Use(auth.AppAuthMiddleware(authDeps))
		uiHandler.RegisterRoutes(r, srv)
	})

	// Self-hosted documentation (embedded Astro/Starlight build).
	r.Handle("/docs", http.RedirectHandler("/docs/", http.StatusMovedPermanently))
	r.Handle("/docs/*", http.StripPrefix("/docs", docs.Handler()))

	// Swagger UI — serves OpenAPI spec and interactive documentation.
	// Uses soft auth so the doc.json fetch from Swagger UI's JavaScript
	// succeeds without requiring a session cookie or bearer token.
	// doc.json is served directly via swag/v2 because http-swagger/v2
	// reads from swag v1, which has a separate registry.
	r.Group(func(r chi.Router) {
		r.Use(auth.AppAuthMiddleware(authDeps))
		r.Get("/swagger/doc.json", swaggerDocJSON)
		r.Get("/swagger/*", httpSwagger.WrapHandler)
	})

	// Auth endpoints — strict rate limit to prevent brute-force.
	r.Group(func(r chi.Router) {
		r.Use(httprate.LimitByIP(10, time.Minute))
		r.Get("/login", auth.LoginHandler(authDeps))
		r.Get("/callback", auth.CallbackHandler(authDeps))
		r.Get("/logout", auth.LogoutHandler(authDeps))
		r.Post("/logout", auth.LogoutHandler(authDeps))
	})

	// Bootstrap token exchange — one-time use, no auth middleware.
	r.Group(func(r chi.Router) {
		r.Use(limitBody)
		r.Use(apiCSP)
		r.Post("/api/v1/bootstrap", ExchangeBootstrapToken(srv))
	})

	// Credential exchange — moderate rate limit.
	r.Group(func(r chi.Router) {
		r.Use(httprate.LimitByIP(20, time.Minute))
		r.Use(limitBody)
		r.Use(apiCSP)
		r.Post("/api/v1/credentials/vault", ExchangeVaultCredential(srv))
	})

	// Proxy routes with app-plane auth middleware (authenticate if possible).
	r.Route("/app", func(sub chi.Router) {
		sub.Use(httprate.LimitByIP(200, time.Minute))
		sub.Use(auth.AppAuthMiddleware(authDeps))
		sub.Get("/{name}", proxy.RedirectTrailingSlash)
		sub.Handle("/{name}/*", proxy.Handler(srv))
	})

	// User-facing API with dual auth (session cookie or bearer token).
	r.Route("/api/v1/users/me", func(r chi.Router) {
		r.Use(httprate.LimitByIP(20, time.Minute))
		r.Use(limitBody)
		r.Use(apiCSP)
		r.Use(UserAuth(srv))
		r.Get("/", GetCurrentUser(srv))
		r.Post("/credentials/{service}", EnrollCredential(srv))
		r.Post("/tokens", CreateToken(srv))
		r.Get("/tokens", ListTokens(srv))
		r.Delete("/tokens", RevokeAllTokens(srv))
		r.Delete("/tokens/{tokenID}", RevokeToken(srv))
	})

	// Worker packages API — authenticated by worker HMAC token.
	r.Route("/api/v1/packages", func(r chi.Router) {
		if srv.WorkerTokenKey != nil {
			r.Use(WorkerAuth(srv.WorkerTokenKey))
		}
		r.Post("/", PostPackages(srv))
	})

	// Authenticated API — general rate limit.
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(httprate.LimitByIP(120, time.Minute))
		r.Use(apiCSP)
		r.Use(APIAuth(srv))

		// Activation endpoint — no request body, registered before limitBody.
		r.Post("/admin/activate", activateHandler(srv, startBG))

		// Bundle upload has its own body size limit (MaxBundleSize),
		// so it is registered before the limitBody middleware below.
		r.Post("/apps/{id}/bundles", UploadBundle(srv))

		// All other API routes get a 1 MiB body size limit.
		r.Group(func(r chi.Router) {
			r.Use(limitBody)

		r.Post("/apps", CreateApp(srv))
		r.Get("/apps", ListAppsV2(srv))
		r.Get("/apps/{id}", GetApp(srv))
		r.Patch("/apps/{id}", UpdateApp(srv))
		r.Delete("/apps/{id}", DeleteApp(srv))

		r.Get("/apps/{id}/bundles", ListBundles(srv))
		r.Get("/apps/{id}/runtime", GetAppRuntime(srv))
		r.Get("/apps/{id}/sessions", ListAppSessions(srv))
		r.Get("/apps/{id}/tags", ListAppTags(srv))

		r.Post("/apps/{id}/rollback", RollbackApp(srv))
		r.Post("/apps/{id}/restore", RestoreApp(srv))

		r.Post("/apps/{id}/enable", EnableApp(srv))
		r.Post("/apps/{id}/disable", DisableApp(srv))
		r.Get("/apps/{id}/logs", AppLogs(srv))

		r.Get("/tasks/{taskID}", GetTaskStatus(srv))
		r.Get("/tasks/{taskID}/logs", TaskLogs(srv))

		// ACL management
		r.Post("/apps/{id}/access", GrantAccess(srv))
		r.Get("/apps/{id}/access", ListAccess(srv))
		r.Delete("/apps/{id}/access/{kind}/{principal}", RevokeAccess(srv))

		// User management
		r.Get("/users", ListUsers(srv))
		r.Get("/users/{sub}", GetUser(srv))
		r.Patch("/users/{sub}", UpdateUser(srv))

		// Tag management
		r.Get("/tags", ListTags(srv))
		r.Post("/tags", CreateTag(srv))
		r.Patch("/tags/{tagID}", RenameTag(srv))
		r.Delete("/tags/{tagID}", DeleteTag(srv))

		// App tag management
		r.Post("/apps/{id}/tags", AddAppTag(srv))
		r.Delete("/apps/{id}/tags/{tagID}", RemoveAppTag(srv))

		// System checks (admin only)
		r.Get("/system/checks", GetSystemChecks(srv))
		r.Post("/system/checks/run", RunSystemChecks(srv))

		// Content discovery (deprecated — use GET /api/v1/apps with search/tag params)
		r.Get("/catalog", CatalogHandler(srv))

		// Deployments
		r.Get("/deployments", ListDeployments(srv))

		// Dependency refresh
		r.Post("/apps/{id}/refresh", PostRefresh(srv))
		r.Post("/apps/{id}/refresh/rollback", PostRefreshRollback(srv))

		}) // end limitBody group
	})

	return r
}

// NewManagementRouter creates the HTTP handler for the management listener.
// It serves /healthz, /readyz, and /metrics without authentication.
// This listener is intended to bind to an internal-only address.
func NewManagementRouter(srv *server.Server) http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if srv.Draining.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("draining"))
			return
		}
		w.Write([]byte("ok"))
	})
	r.Get("/readyz", readyzHandler(srv, true))

	if srv.Config.Telemetry != nil && srv.Config.Telemetry.MetricsEnabled {
		r.Handle("/metrics", promhttp.Handler())
	}

	return r
}
