package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/proxy"
	"github.com/cynkra/blockyard/internal/server"
)

func NewRouter(srv *server.Server) http.Handler {
	r := chi.NewRouter()

	authDeps := srv.AuthDeps()

	// Unauthenticated
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	// Auth endpoints (outside app-plane auth layer).
	r.Get("/login", auth.LoginHandler(authDeps))
	r.Get("/callback", auth.CallbackHandler(authDeps))
	r.Post("/logout", auth.LogoutHandler(authDeps))

	// Proxy routes with app-plane auth middleware (authenticate if possible).
	r.Route("/app", func(sub chi.Router) {
		sub.Use(auth.AppAuthMiddleware(authDeps, srv.RoleCache))
		sub.Get("/{name}", proxy.RedirectTrailingSlash)
		sub.Handle("/{name}/*", proxy.Handler(srv))
	})

	// User-facing API with dual auth (session cookie or JWT bearer).
	r.Route("/api/v1/users/me", func(r chi.Router) {
		r.Use(UserAuth(srv))
		r.Post("/credentials/{service}", EnrollCredential(srv))
	})

	// Authenticated API
	r.Route("/api/v1", func(r chi.Router) {
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
	})

	return r
}
