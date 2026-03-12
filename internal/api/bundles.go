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

		// 1. Validate app exists (resolve by UUID then name)
		app, err := resolveApp(srv.DB, appID)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}
		if app == nil {
			notFound(w, "app "+appID+" not found")
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
			serverError(w, "write archive: "+err.Error())
			return
		}

		// 5. Unpack
		if err := bundle.UnpackArchive(paths); err != nil {
			bundle.DeleteFiles(paths)
			serverError(w, "unpack: "+err.Error())
			return
		}

		// 6. Validate entrypoint
		if err := bundle.ValidateEntrypoint(paths); err != nil {
			bundle.DeleteFiles(paths)
			badRequest(w, err.Error())
			return
		}

		// 7. Create library dir
		if err := bundle.CreateLibraryDir(paths); err != nil {
			bundle.DeleteFiles(paths)
			serverError(w, "create library dir: "+err.Error())
			return
		}

		// 8. Insert bundle row (status = pending)
		if _, err := srv.DB.CreateBundle(bundleID, app.ID); err != nil {
			bundle.DeleteFiles(paths)
			serverError(w, "create bundle row: "+err.Error())
			return
		}

		// 9. Create task in TaskStore
		sender := srv.Tasks.Create(taskID)

		// 10. Spawn async restore
		bundle.SpawnRestore(bundle.RestoreParams{
			Backend:      srv.Backend,
			DB:           srv.DB,
			Tasks:        srv.Tasks,
			Sender:       sender,
			AppID:        app.ID,
			BundleID:     bundleID,
			Paths:        paths,
			Image:        srv.Config.Docker.Image,
			RvVersion:    srv.Config.Docker.RvVersion,
			RvBinaryPath: srv.Config.Docker.RvBinaryPath,
			Retention:    srv.Config.Storage.BundleRetention,
			BasePath:     srv.Config.Storage.BundleServerPath,
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

		// Resolve app by UUID then name to get the canonical ID
		app, err := resolveApp(srv.DB, appID)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}
		if app == nil {
			notFound(w, "app "+appID+" not found")
			return
		}

		bundles, err := srv.DB.ListBundlesByApp(app.ID)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(bundles)
	}
}
