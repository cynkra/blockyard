package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

// handleBlockyardInternal handles requests under the /__blockyard/
// reserved prefix. Returns true if the request was handled (caller
// should return), false if the request should proceed to normal proxy
// routing.
func handleBlockyardInternal(
	w http.ResponseWriter,
	r *http.Request,
	app *db.AppRow,
	appName string,
	srv *server.Server,
) bool {
	path := r.URL.Path
	prefix := "/app/" + appName + "/"
	remainder := strings.TrimPrefix(path, prefix)

	if !strings.HasPrefix(remainder, "__blockyard/") {
		return false
	}

	endpoint := strings.TrimPrefix(remainder, "__blockyard/")

	switch endpoint {
	case "ready":
		handleReady(w, r, app, srv)
	default:
		http.NotFound(w, r)
	}
	return true
}

type readyResponse struct {
	Ready bool `json:"ready"`
}

// handleReady responds with whether the app has at least one healthy
// available (non-draining) worker.
func handleReady(
	w http.ResponseWriter,
	r *http.Request,
	app *db.AppRow,
	srv *server.Server,
) {
	ready := false
	for _, wid := range srv.Workers.ForAppAvailable(app.ID) {
		if srv.Backend.HealthCheck(r.Context(), wid) {
			ready = true
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(readyResponse{Ready: ready})
}
