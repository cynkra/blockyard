package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/server"
)

func UploadBundle(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		appID := chi.URLParam(r, "id")

		// 1. Validate app exists
		app, err := srv.DB.GetApp(appID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error",
				"db error: "+err.Error())
			return
		}
		if app == nil {
			writeError(w, http.StatusNotFound, "not_found",
				"app "+appID+" not found")
			return
		}

		// 2. Enforce body size limit
		r.Body = http.MaxBytesReader(w, r.Body, srv.Config.Storage.MaxBundleSize)

		// 3. Generate IDs
		bundleID := uuid.New().String()
		taskID := uuid.New().String()
		slog.Info("bundle upload started", "app_id", appID, "bundle_id", bundleID)

		// 4. Stream archive to disk
		paths := bundle.NewBundlePaths(srv.Config.Storage.BundleServerPath, app.ID, bundleID)
		if err := bundle.WriteArchive(paths, r.Body); err != nil {
			// Check if this was a size limit error
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
					"bundle exceeds max_bundle_size")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal_error",
				"write archive: "+err.Error())
			return
		}

		// 5. Unpack
		if err := bundle.UnpackArchive(paths); err != nil {
			bundle.DeleteFiles(paths)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"unpack: "+err.Error())
			return
		}

		// 6. Create library dir
		if err := bundle.CreateLibraryDir(paths); err != nil {
			bundle.DeleteFiles(paths)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"create library dir: "+err.Error())
			return
		}

		// 7. Insert bundle row (status = pending)
		if _, err := srv.DB.CreateBundle(bundleID, app.ID); err != nil {
			bundle.DeleteFiles(paths)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"create bundle row: "+err.Error())
			return
		}

		// 8. Create task in TaskStore
		sender := srv.Tasks.Create(taskID)

		// 9. Spawn async restore
		bundle.SpawnRestore(bundle.RestoreParams{
			Backend:   srv.Backend,
			DB:        srv.DB,
			Tasks:     srv.Tasks,
			Sender:    sender,
			AppID:     app.ID,
			BundleID:  bundleID,
			Paths:     paths,
			Image:     srv.Config.Docker.Image,
			RvVersion: srv.Config.Docker.RvVersion,
			Retention: srv.Config.Storage.BundleRetention,
			BasePath:  srv.Config.Storage.BundleServerPath,
		})

		// 10. Return 202
		slog.Info("bundle upload accepted, restore spawned",
			"app_id", appID, "bundle_id", bundleID, "task_id", taskID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{
			"bundle_id": bundleID,
			"task_id":   taskID,
		})
	}
}

func ListBundles(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		appID := chi.URLParam(r, "id")

		bundles, err := srv.DB.ListBundlesByApp(appID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error",
				"db error: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(bundles)
	}
}
