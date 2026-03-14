package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/proxy"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/telemetry"
	"github.com/cynkra/blockyard/internal/ui"
)

// securityHeaders is a middleware that sets common security response headers.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

func NewRouter(srv *server.Server) http.Handler {
	r := chi.NewRouter()

	// Global security headers.
	r.Use(securityHeaders)

	// OpenTelemetry tracing middleware (only when configured).
	if srv.Config.Telemetry != nil && srv.Config.Telemetry.OTLPEndpoint != "" {
		r.Use(telemetry.TracingMiddleware())
	}

	authDeps := srv.AuthDeps()

	// Unauthenticated
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	r.Get("/readyz", readyzHandler(srv))

	// Prometheus metrics endpoint (only when enabled, requires API auth).
	if srv.Config.Telemetry != nil && srv.Config.Telemetry.MetricsEnabled {
		r.Group(func(r chi.Router) {
			r.Use(APIAuth(srv))
			r.Handle("/metrics", promhttp.Handler())
		})
	}

	// UI routes — soft auth populates session context if available.
	uiHandler := ui.New()
	r.Group(func(r chi.Router) {
		r.Use(auth.AppAuthMiddleware(authDeps, srv.RoleCache))
		uiHandler.RegisterRoutes(r, srv)
	})

	// Auth endpoints — strict rate limit to prevent brute-force.
	r.Group(func(r chi.Router) {
		r.Use(httprate.LimitByIP(10, time.Minute))
		r.Get("/login", auth.LoginHandler(authDeps))
		r.Get("/callback", auth.CallbackHandler(authDeps))
		r.Post("/logout", auth.LogoutHandler(authDeps))
	})

	// Credential exchange — moderate rate limit.
	r.Group(func(r chi.Router) {
		r.Use(httprate.LimitByIP(20, time.Minute))
		r.Post("/api/v1/credentials/vault", ExchangeVaultCredential(srv))
	})

	// Proxy routes with app-plane auth middleware (authenticate if possible).
	r.Route("/app", func(sub chi.Router) {
		sub.Use(httprate.LimitByIP(200, time.Minute))
		sub.Use(auth.AppAuthMiddleware(authDeps, srv.RoleCache))
		sub.Get("/{name}", proxy.RedirectTrailingSlash)
		sub.Handle("/{name}/*", proxy.Handler(srv))
	})

	// User-facing API with dual auth (session cookie or JWT bearer).
	r.Route("/api/v1/users/me", func(r chi.Router) {
		r.Use(httprate.LimitByIP(20, time.Minute))
		r.Use(UserAuth(srv))
		r.Post("/credentials/{service}", EnrollCredential(srv))
	})

	// Authenticated API — general rate limit.
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(httprate.LimitByIP(120, time.Minute))
		r.Use(APIAuth(srv))

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

		// ACL management
		r.Post("/apps/{id}/access", GrantAccess(srv))
		r.Get("/apps/{id}/access", ListAccess(srv))
		r.Delete("/apps/{id}/access/{kind}/{principal}", RevokeAccess(srv))

		// Role mapping management
		r.Get("/role-mappings", ListRoleMappings(srv))
		r.Put("/role-mappings/{group_name}", SetRoleMapping(srv))
		r.Delete("/role-mappings/{group_name}", DeleteRoleMapping(srv))

		// Tag management
		r.Get("/tags", ListTags(srv))
		r.Post("/tags", CreateTag(srv))
		r.Delete("/tags/{tagID}", DeleteTag(srv))

		// App tag management
		r.Post("/apps/{id}/tags", AddAppTag(srv))
		r.Delete("/apps/{id}/tags/{tagID}", RemoveAppTag(srv))

		// Content discovery
		r.Get("/catalog", CatalogHandler(srv))
	})

	return r
}
