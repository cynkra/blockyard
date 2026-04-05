package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/telemetry"
)

// UploadBundle uploads a new bundle archive for an app.
//
//	@Summary		Upload bundle
//	@Description	Upload a tar.gz bundle archive. Triggers async restore (package installation). Track progress via the returned task_id.
//	@Tags			bundles
//	@Accept			application/gzip
//	@Produce		json
//	@Param			id	path		string	true	"App ID (UUID) or name"
//	@Success		202	{object}	uploadBundleResponse
//	@Failure		400	{object}	errorResponse
//	@Failure		404	{object}	errorResponse
//	@Failure		413	{object}	errorResponse
//	@Failure		500	{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/bundles [post]
func UploadBundle(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		appID := chi.URLParam(r, "id")

		app, relation, ok := resolveAppRelation(srv, w, caller, appID)
		if !ok {
			return
		}

		if !relation.CanDeploy() {
			notFound(w, "app not found")
			return
		}

		// Enforce body size limit
		r.Body = http.MaxBytesReader(w, r.Body, srv.Config.Storage.MaxBundleSize)

		// Generate IDs
		bundleID := uuid.New().String()
		taskID := uuid.New().String()
		slog.Info("bundle upload started", "app_id", appID, "bundle_id", bundleID) //nolint:gosec // G706: slog structured logging handles this

		// Stream archive to disk
		paths := bundle.NewBundlePaths(srv.Config.Storage.BundleServerPath, app.ID, bundleID)
		if err := bundle.WriteArchive(paths, r.Body); err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
					"bundle exceeds max_bundle_size")
				return
			}
			serverError(w, "write archive: "+err.Error())
			return
		}

		// Unpack
		if err := bundle.UnpackArchive(paths); err != nil {
			bundle.DeleteFiles(paths)
			serverError(w, "unpack: "+err.Error())
			return
		}

		// Validate entrypoint
		if err := bundle.ValidateEntrypoint(paths); err != nil {
			bundle.DeleteFiles(paths)
			badRequest(w, err.Error())
			return
		}

		// Create library dir
		if err := bundle.CreateLibraryDir(paths); err != nil {
			bundle.DeleteFiles(paths)
			serverError(w, "create library dir: "+err.Error())
			return
		}

		// Insert bundle row (status = pending)
		// Determine if bundle has pinned dependencies by checking the manifest.
		bundlePinned := false
		manifestPath := filepath.Join(paths.Unpacked, "manifest.json")
		if m, mErr := manifest.Read(manifestPath); mErr == nil {
			bundlePinned = m.IsPinned()
		}
		deployedBy := ""
		if caller != nil {
			deployedBy = caller.Sub
		}
		if _, err := srv.DB.CreateBundle(bundleID, app.ID, deployedBy, bundlePinned); err != nil {
			bundle.DeleteFiles(paths)
			serverError(w, "create bundle row: "+err.Error())
			return
		}

		// Create task in TaskStore
		sender := srv.Tasks.Create(taskID, app.ID)

		// Spawn async restore
		actorSub := "anonymous"
		if caller != nil {
			actorSub = caller.Sub
		}
		bundle.SpawnRestore(bundle.RestoreParams{
			Backend:          srv.Backend,
			DB:               srv.DB,
			Tasks:            srv.Tasks,
			Sender:           sender,
			AppID:            app.ID,
			BundleID:         bundleID,
			Paths:            paths,
			Image:            server.AppImage(app, srv.Config.Docker.Image),
			PakVersion:       srv.Config.Docker.PakVersion,
			PakCachePath:     filepath.Join(srv.Config.Storage.BundleServerPath, ".pak-cache"),
			BuilderVersion:   srv.Version,
			BuilderCachePath: filepath.Join(srv.Config.Storage.BundleServerPath, ".by-builder-cache"),
			Retention:        srv.Config.Storage.BundleRetention,
			BasePath:         srv.Config.Storage.BundleServerPath,
			Store:            srv.PkgStore,
			AuditLog:         srv.AuditLog,
			AuditActor:       actorSub,
			WG:               srv.RestoreWG,
		})

		telemetry.BundlesUploaded.Inc()
		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionBundleUpload, app.ID,
				map[string]any{"bundle_id": bundleID}))
		}

		slog.Info("bundle upload accepted, restore spawned", //nolint:gosec // G706: slog structured logging handles this
			"app_id", appID, "bundle_id", bundleID, "task_id", taskID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{
			"bundle_id": bundleID,
			"task_id":   taskID,
		})
	}
}

// ListBundles lists all bundles for an app.
//
//	@Summary		List bundles
//	@Description	List all bundles for an app, ordered by upload time.
//	@Tags			bundles
//	@Produce		json
//	@Param			id	path		string	true	"App ID (UUID) or name"
//	@Success		200	{object}	bundleListResponse
//	@Failure		404	{object}	errorResponse
//	@Failure		500	{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/bundles [get]
func ListBundles(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		appID := chi.URLParam(r, "id")

		app, relation, ok := resolveAppRelation(srv, w, caller, appID)
		if !ok {
			return
		}

		// RBAC: collaborator+ required (tightened from "any access").
		if !relation.CanDeploy() {
			notFound(w, "app not found")
			return
		}

		bundles, err := srv.DB.ListBundlesByApp(app.ID)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"bundles": bundles})
	}
}
