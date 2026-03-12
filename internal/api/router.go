package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/server"
)

func NewRouter(srv *server.Server) http.Handler {
	r := chi.NewRouter()

	// Unauthenticated
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	// Authenticated API
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(BearerAuth(srv))

		r.Post("/apps/{id}/bundles", UploadBundle(srv))
		r.Get("/apps/{id}/bundles", ListBundles(srv))
		r.Get("/tasks/{taskID}", GetTaskStatus(srv))
		r.Get("/tasks/{taskID}/logs", TaskLogs(srv))
	})

	return r
}
