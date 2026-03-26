package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/server"
)

// PostRefresh starts a dependency refresh for an unpinned deployment.
func PostRefresh(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		appID := chi.URLParam(r, "id")
		caller := auth.CallerFromContext(r.Context())

		app, _, ok := resolveAppRelation(srv, w, caller, appID)
		if !ok {
			return
		}

		if app.ActiveBundle == nil {
			writeJSON(w, http.StatusConflict,
				map[string]string{"message": "app has no active bundle"})
			return
		}

		// Only unpinned deployments can be refreshed.
		manifestPath := filepath.Join(
			srv.BundlePaths(app.ID, *app.ActiveBundle).Base,
			"manifest.json")
		m, err := manifest.Read(manifestPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError,
				map[string]string{"message": "read manifest: " + err.Error()})
			return
		}
		if m.IsPinned() {
			writeJSON(w, http.StatusConflict,
				map[string]string{
					"message": "app was deployed with pinned dependencies; " +
						"redeploy to update",
				})
			return
		}

		// Start refresh as a background task.
		taskID := uuid.New().String()
		sender := srv.Tasks.Create(taskID, app.ID)
		go srv.RunRefresh(context.Background(), app, m, sender)

		writeJSON(w, http.StatusAccepted, map[string]string{
			"task_id": taskID,
			"message": "refresh started",
		})
	}
}

// PostRefreshRollback rolls back to a previous refresh or the original build.
func PostRefreshRollback(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		appID := chi.URLParam(r, "id")
		caller := auth.CallerFromContext(r.Context())

		app, _, ok := resolveAppRelation(srv, w, caller, appID)
		if !ok {
			return
		}

		if app.ActiveBundle == nil {
			writeJSON(w, http.StatusConflict,
				map[string]string{"message": "app has no active bundle"})
			return
		}

		// ?target=build rolls back to original deploy; default is previous refresh.
		target := r.URL.Query().Get("target")

		// Validate rollback target before starting the async task.
		bundlePaths := srv.BundlePaths(app.ID, *app.ActiveBundle)
		switch target {
		case "build":
			buildManifest := filepath.Join(bundlePaths.Base, "store-manifest.json.build")
			if !fileExists(buildManifest) {
				writeJSON(w, http.StatusConflict,
					map[string]string{"message": "no build store-manifest available"})
				return
			}
		default:
			prevManifest := filepath.Join(bundlePaths.Base, "store-manifest.json.prev")
			if !fileExists(prevManifest) {
				writeJSON(w, http.StatusConflict,
					map[string]string{
						"message": "no previous refresh to roll back to " +
							"(use ?target=build to roll back to the original deploy)",
					})
				return
			}
		}

		// Start rollback as a background task.
		taskID := uuid.New().String()
		sender := srv.Tasks.Create(taskID, app.ID)
		go srv.RunRollback(context.Background(), app, target, sender)

		writeJSON(w, http.StatusAccepted, map[string]string{
			"task_id": taskID,
			"message": "rollback started",
		})
	}
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
