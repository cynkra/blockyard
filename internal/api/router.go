package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/proxy"
	"github.com/cynkra/blockyard/internal/server"
)

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
