package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/appname"
	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/authz"
	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/mount"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/units"
)

// AppResponse wraps an AppRow with a derived runtime status.
type AppResponse struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Owner                string   `json:"owner"`
	AccessType           string   `json:"access_type"`
	ActiveBundle         *string  `json:"active_bundle"`
	MaxWorkersPerApp     *int     `json:"max_workers_per_app"`
	MaxSessionsPerWorker int      `json:"max_sessions_per_worker"`
	MemoryLimit          *string  `json:"memory_limit"`
	CPULimit             *float64 `json:"cpu_limit"`
	Title                *string  `json:"title"`
	Description          *string  `json:"description"`
	PreWarmedSessions    int      `json:"pre_warmed_sessions"`
	CreatedAt            string   `json:"created_at"`
	UpdatedAt            string   `json:"updated_at"`
	DeletedAt            *string  `json:"deleted_at,omitempty"`
	Status               string   `json:"status"`
	Workers              []string `json:"workers"`
}

func appResponse(app *db.AppRow, workers server.WorkerMap) AppResponse {
	status := "stopped"
	workerIDs := workers.ForApp(app.ID)
	if len(workerIDs) > 0 {
		status = "running"
	}
	if workerIDs == nil {
		workerIDs = []string{}
	}
	return AppResponse{
		ID:                   app.ID,
		Name:                 app.Name,
		Owner:                app.Owner,
		AccessType:           app.AccessType,
		ActiveBundle:         app.ActiveBundle,
		MaxWorkersPerApp:     app.MaxWorkersPerApp,
		MaxSessionsPerWorker: app.MaxSessionsPerWorker,
		MemoryLimit:          app.MemoryLimit,
		CPULimit:             app.CPULimit,
		Title:                app.Title,
		Description:          app.Description,
		PreWarmedSessions:    app.PreWarmedSessions,
		CreatedAt:            app.CreatedAt,
		UpdatedAt:            app.UpdatedAt,
		DeletedAt:            app.DeletedAt,
		Status:               status,
		Workers:              workerIDs,
	}
}

// resolveApp looks up an app by UUID first, then by name.
func resolveApp(database *db.DB, id string) (*db.AppRow, error) {
	app, err := database.GetApp(id)
	if err != nil {
		return nil, err
	}
	if app != nil {
		return app, nil
	}
	app, err = database.GetAppByName(id)
	if err != nil {
		return nil, err
	}
	if app != nil {
		return app, nil
	}
	// Try alias table as a final fallback (supports app renames).
	app, _, err = database.GetAppByAlias(id)
	return app, err
}

// resolveAppRelation loads an app + ACL grants, evaluates the caller's
// relationship. Returns the app and relation, or writes an error response
// and returns false.
//
// Returns 404 both when the app doesn't exist and when the caller has no
// access — this prevents leaking app existence to unauthorized users.
func resolveAppRelation(
	srv *server.Server,
	w http.ResponseWriter,
	caller *auth.CallerIdentity,
	appID string,
) (*db.AppRow, authz.AppRelation, bool) {
	app, err := resolveApp(srv.DB, appID)
	if err != nil {
		serverError(w, "db error: "+err.Error())
		return nil, authz.RelationNone, false
	}
	if app == nil {
		notFound(w, "app not found")
		return nil, authz.RelationNone, false
	}

	rows, err := srv.DB.ListAppAccess(app.ID)
	if err != nil {
		serverError(w, "db error: "+err.Error())
		return nil, authz.RelationNone, false
	}

	grants := make([]authz.AccessGrant, len(rows))
	for i, row := range rows {
		grants[i] = accessRowToGrant(row)
	}

	relation := authz.EvaluateAccess(caller, app.Owner, grants, app.AccessType)

	if relation == authz.RelationNone {
		notFound(w, "app not found")
		return nil, authz.RelationNone, false
	}

	return app, relation, true
}

// accessRowToGrant converts a db.AppAccessRow to an authz.AccessGrant.
func accessRowToGrant(row db.AppAccessRow) authz.AccessGrant {
	role, _ := authz.ParseContentRole(row.Role)
	return authz.AccessGrant{
		AppID:     row.AppID,
		Principal: row.Principal,
		Kind:      authz.AccessKind(row.Kind),
		Role:      role,
		GrantedBy: row.GrantedBy,
		GrantedAt: row.GrantedAt,
	}
}

// validateAppName checks that name is a valid URL-safe slug.
func validateAppName(name string) error {
	return appname.Validate(name)
}

type createAppRequest struct {
	Name string `json:"name"`
}

// CreateApp creates a new application.
//
//	@Summary		Create app
//	@Description	Create a new application owned by the caller.
//	@Tags			apps
//	@Accept			json
//	@Produce		json
//	@Param			body	body		createAppRequest	true	"App name"
//	@Success		201		{object}	AppResponse
//	@Failure		400		{object}	errorResponse
//	@Failure		403		{object}	errorResponse
//	@Failure		409		{object}	errorResponse
//	@Failure		500		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps [post]
func CreateApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanCreateApp() {
			forbidden(w, "insufficient permissions")
			return
		}

		var body createAppRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid JSON body")
			return
		}

		if err := validateAppName(body.Name); err != nil {
			badRequest(w, err.Error())
			return
		}

		existing, err := srv.DB.GetAppByName(body.Name)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}
		if existing != nil {
			conflict(w, fmt.Sprintf("app name %q already exists", body.Name))
			return
		}

		app, err := srv.DB.CreateApp(body.Name, caller.Sub)
		if err != nil {
			serverError(w, "create app: "+err.Error())
			return
		}

		slog.Info("app created",
			"app_id", app.ID, "name", app.Name, "owner", caller.Sub)

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionAppCreate, app.ID,
				map[string]any{"name": app.Name}))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(appResponseV2(app, srv))
	}
}

func ListApps(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())

		var apps []db.AppRow
		var err error

		if caller == nil {
			forbidden(w, "insufficient permissions")
			return
		}

		// ?deleted=true — admin-only, returns soft-deleted apps
		if r.URL.Query().Get("deleted") == "true" {
			if !caller.Role.CanViewAllApps() {
				forbidden(w, "admin only")
				return
			}
			apps, err = srv.DB.ListDeletedApps()
		} else if caller.Role.CanViewAllApps() {
			apps, err = srv.DB.ListApps()
		} else {
			apps, err = srv.DB.ListAccessibleApps(caller.Sub)
		}
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}

		responses := make([]AppResponse, len(apps))
		for i, app := range apps {
			responses[i] = appResponse(&app, srv.Workers)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

// GetApp returns a single application by ID or name.
//
//	@Summary		Get app
//	@Description	Get a single application by UUID or name. Returns 404 if not found or caller has no access.
//	@Tags			apps
//	@Produce		json
//	@Param			id	path		string	true	"App ID (UUID) or name"
//	@Success		200	{object}	appResponseV2JSON
//	@Failure		404	{object}	errorResponse
//	@Failure		500	{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id} [get]
func GetApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		app, relation, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}

		resp := appResponseV2WithRelation(app, srv, relation.String())
		dataMounts, _ := srv.DB.ListAppDataMounts(app.ID)
		if dataMounts == nil {
			dataMounts = []db.DataMountRow{}
		}
		resp["data_mounts"] = dataMounts

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

type dataMountInput struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly *bool  `json:"readonly"` // defaults to true when omitted
}

type updateAppRequest struct {
	Name                 *string          `json:"name"`
	MaxWorkersPerApp     *int             `json:"max_workers_per_app"`
	MaxSessionsPerWorker *int             `json:"max_sessions_per_worker"`
	MemoryLimit          *string          `json:"memory_limit"`
	CPULimit             *float64         `json:"cpu_limit"`
	AccessType           *string          `json:"access_type"`
	Title                *string          `json:"title"`
	Description          *string          `json:"description"`
	PreWarmedSessions    *int             `json:"pre_warmed_sessions"`
	RefreshSchedule      *string          `json:"refresh_schedule"`
	Image                *string          `json:"image"`
	Runtime              *string          `json:"runtime"`
	DataMounts           []dataMountInput `json:"data_mounts,omitempty"`
}

// UpdateApp updates an application's configuration.
//
//	@Summary		Update app
//	@Description	Update an application's settings. All fields are optional. Changing access_type requires owner/admin.
//	@Tags			apps
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string				true	"App ID (UUID) or name"
//	@Param			body	body		updateAppRequest	true	"Fields to update"
//	@Success		200		{object}	appResponseV2JSON
//	@Failure		400		{object}	errorResponse
//	@Failure		404		{object}	errorResponse
//	@Failure		500		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id} [patch]
func UpdateApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		var body updateAppRequest
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			// htmx sends form-encoded by default.
			if err := r.ParseForm(); err != nil { //nolint:gosec // G120: auth-gated endpoint
				badRequest(w, "invalid form data")
				return
			}
			body = parseUpdateAppForm(r)
		} else {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				badRequest(w, "invalid JSON body")
				return
			}
		}

		if body.MaxSessionsPerWorker != nil && *body.MaxSessionsPerWorker < 1 {
			badRequest(w, "max_sessions_per_worker must be >= 1")
			return
		}
		if body.MaxWorkersPerApp != nil && *body.MaxWorkersPerApp < 1 {
			badRequest(w, "max_workers_per_app must be >= 1")
			return
		}
		if body.MemoryLimit != nil && *body.MemoryLimit != "" {
			if _, ok := units.ParseMemoryLimit(*body.MemoryLimit); !ok {
				badRequest(w, `invalid memory_limit format: use e.g. "256m", "1g", "512mb"`)
				return
			}
		}
		if body.CPULimit != nil {
			if *body.CPULimit < 0 {
				badRequest(w, "cpu_limit must be non-negative")
				return
			}
			if srv.Config.Proxy.MaxCPULimit != nil && *srv.Config.Proxy.MaxCPULimit > 0 && *body.CPULimit > *srv.Config.Proxy.MaxCPULimit {
				badRequest(w, fmt.Sprintf("cpu_limit must not exceed %.1f", *srv.Config.Proxy.MaxCPULimit))
				return
			}
		}
		// The process backend cannot enforce per-worker resource
		// limits — there is no cgroup delegation, so memory and CPU
		// caps set on individual apps would be silently ignored at
		// spawn time. Warn the user *here*, at the moment they're
		// setting the value, rather than emitting one warning per
		// worker spawn for the lifetime of the deployment.
		settingLimit := (body.MemoryLimit != nil && *body.MemoryLimit != "") ||
			(body.CPULimit != nil && *body.CPULimit > 0)
		if settingLimit && srv.Config.Server.Backend == "process" {
			msg := "process backend does not enforce per-worker memory_limit/cpu_limit; the value will be stored but ignored at spawn time. Use the docker backend if you need cgroup-enforced limits."
			slog.Warn("api: per-app resource limit ignored on process backend", //nolint:gosec // G706: slog structured logging handles this
				"app_id", id,
				"memory_limit", stringOrEmpty(body.MemoryLimit),
				"cpu_limit", floatOrZero(body.CPULimit))
			w.Header().Add("X-Blockyard-Warning", msg)
		}
		if body.PreWarmedSessions != nil {
			if *body.PreWarmedSessions < 0 {
				badRequest(w, "pre_warmed_sessions must be non-negative")
				return
			}
			if *body.PreWarmedSessions > 10 {
				badRequest(w, "pre_warmed_sessions must not exceed 10")
				return
			}
		}

		app, relation, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}

		if !relation.CanUpdateConfig() {
			notFound(w, "app not found")
			return
		}

		// Changing access_type requires ACL management permission (owner or admin).
		if body.AccessType != nil {
			if !relation.CanManageACL() {
				notFound(w, "app not found")
				return
			}
			if *body.AccessType != "acl" && *body.AccessType != "logged_in" && *body.AccessType != "public" {
				badRequest(w, "access_type must be 'acl', 'logged_in', or 'public'")
				return
			}
		}

		// Validate refresh_schedule if provided.
		if body.RefreshSchedule != nil && *body.RefreshSchedule != "" {
			if !isValidCron(*body.RefreshSchedule) {
				if r.Header.Get("HX-Request") != "" {
					w.WriteHeader(http.StatusUnprocessableEntity)
					w.Write([]byte("invalid cron expression"))
					return
				}
				badRequest(w, "invalid refresh_schedule cron expression")
				return
			}
		}

		if body.Image != nil && *body.Image != "" {
			img := *body.Image
			if strings.ContainsAny(img, " \t\n") {
				badRequest(w, "image must not contain whitespace")
				return
			}
		}

		// Runtime changes require admin — runtime controls the container
		// isolation boundary (e.g., runc vs kata vs sysbox).
		if body.Runtime != nil && (caller == nil || !caller.Role.CanManageRoles()) {
			forbidden(w, "runtime requires admin")
			return
		}

		// Convert dataMountInput → DataMountRow, defaulting ReadOnly to true.
		var mountRows []db.DataMountRow
		if body.DataMounts != nil {
			mountRows = make([]db.DataMountRow, len(body.DataMounts))
			for i, m := range body.DataMounts {
				ro := true
				if m.ReadOnly != nil {
					ro = *m.ReadOnly
				}
				mountRows[i] = db.DataMountRow{
					Source:   m.Source,
					Target:   m.Target,
					ReadOnly: ro,
				}
			}
			if err := mount.Validate(mountRows, srv.Config.Storage.DataMounts); err != nil {
				badRequest(w, fmt.Sprintf("data_mounts: %v", err))
				return
			}
		}

		// Handle rename separately — it has its own transaction.
		if body.Name != nil && *body.Name != app.Name {
			if !relation.CanManageACL() { // owner or admin only
				notFound(w, "app not found")
				return
			}
			newName := *body.Name
			if err := validateAppName(newName); err != nil {
				badRequest(w, err.Error())
				return
			}
			oldName := app.Name
			if err := srv.DB.RenameApp(app.ID, oldName, newName); err != nil {
				if db.IsUniqueConstraintError(err) {
					badRequest(w, "name already in use")
					return
				}
				serverError(w, "rename app: "+err.Error())
				return
			}
			if srv.AuditLog != nil {
				srv.AuditLog.Emit(auditEntry(r, audit.ActionAppRename, app.ID,
					map[string]any{"old_name": oldName, "new_name": newName}))
			}
		}

		update := db.AppUpdate{
			MaxWorkersPerApp:     body.MaxWorkersPerApp,
			MaxSessionsPerWorker: body.MaxSessionsPerWorker,
			MemoryLimit:          body.MemoryLimit,
			CPULimit:             body.CPULimit,
			AccessType:           body.AccessType,
			Title:                body.Title,
			Description:          body.Description,
			PreWarmedSessions:    body.PreWarmedSessions,
			RefreshSchedule:      body.RefreshSchedule,
			Image:                body.Image,
			Runtime:              body.Runtime,
		}
		updated, err := srv.DB.UpdateApp(app.ID, update)
		if err != nil {
			serverError(w, "update app: "+err.Error())
			return
		}

		if mountRows != nil {
			for i := range mountRows {
				mountRows[i].AppID = app.ID
			}
			if err := srv.DB.SetAppDataMounts(app.ID, mountRows); err != nil {
				serverError(w, "set data mounts: "+err.Error())
				return
			}
		}

		// Live-update resource limits on running workers (best-effort).
		if body.MemoryLimit != nil || body.CPULimit != nil {
			mem := int64(0)
			if updated.MemoryLimit != nil {
				if parsed, ok := units.ParseMemoryLimit(*updated.MemoryLimit); ok {
					mem = parsed
				}
			}
			cpuNano := int64(0)
			if updated.CPULimit != nil {
				cpuNano = int64(*updated.CPULimit * 1e9)
			}
			for _, wid := range srv.Workers.ForApp(app.ID) {
				err := srv.Backend.UpdateResources(r.Context(), wid, mem, cpuNano)
				if err != nil && !errors.Is(err, backend.ErrNotSupported) {
					slog.Warn("failed to update worker resources",
						"worker", wid, "error", err)
				}
			}
		}

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionAppUpdate, app.ID, nil))
		}

		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Trigger", "appUpdated")
		}

		resp := appResponseV2(updated, srv)
		dataMounts, _ := srv.DB.ListAppDataMounts(app.ID)
		if dataMounts == nil {
			dataMounts = []db.DataMountRow{}
		}
		resp["data_mounts"] = dataMounts

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// DeleteApp soft-deletes an application (or hard-deletes with ?purge=true, admin only).
//
//	@Summary		Delete app
//	@Description	Soft-delete an app, stopping all workers. Use ?purge=true for permanent deletion (admin only, app must be soft-deleted first).
//	@Tags			apps
//	@Param			id		path	string	true	"App ID (UUID) or name"
//	@Param			purge	query	bool	false	"Permanently delete (admin only)"
//	@Success		204		"Deleted"
//	@Failure		403		{object}	errorResponse
//	@Failure		404		{object}	errorResponse
//	@Failure		409		{object}	errorResponse
//	@Failure		500		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id} [delete]
func DeleteApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		// Handle ?purge=true — admin-only hard delete.
		if r.URL.Query().Get("purge") == "true" {
			HardDeleteApp(srv, true, w, r, caller, id)
			return
		}

		app, relation, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}

		if !relation.CanDelete() {
			notFound(w, "app not found")
			return
		}

		slog.Info("deleting app",
			"app_id", app.ID, "name", app.Name, "caller", caller.Sub)

		// Always stop running workers.
		ops.StopAppSync(srv, app.ID)

		if srv.Config.Storage.SoftDeleteRetention.Duration > 0 {
			// Soft-delete: mark as deleted, retain files and rows.
			if err := srv.DB.SoftDeleteApp(app.ID); err != nil {
				serverError(w, "soft delete: "+err.Error())
				return
			}
		} else {
			// Immediate hard delete (legacy behavior).
			ops.PurgeApp(srv, app)
		}

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionAppDelete, app.ID,
				map[string]any{"name": app.Name}))
		}

		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Trigger", `{"showToast":{"message":"App deleted","type":"success"}}`)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type rollbackRequest struct {
	BundleID string `json:"bundle_id"`
}

// RollbackApp switches an app to a previous bundle.
//
//	@Summary		Rollback app bundle
//	@Description	Switch an app's active bundle to a previous one. Stops running workers and activates the target bundle.
//	@Tags			apps
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string			true	"App ID (UUID) or name"
//	@Param			body	body		rollbackRequest	true	"Target bundle"
//	@Success		200		{object}	AppResponse
//	@Failure		400		{object}	errorResponse
//	@Failure		404		{object}	errorResponse
//	@Failure		500		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/rollback [post]
func RollbackApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		var body rollbackRequest
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			_ = r.ParseForm() //nolint:gosec // G120: auth-gated endpoint
			body.BundleID = r.FormValue("bundle_id")
		} else {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				badRequest(w, "invalid JSON body")
				return
			}
		}

		if body.BundleID == "" {
			badRequest(w, "bundle_id is required")
			return
		}

		app, relation, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}
		if !relation.CanDeploy() {
			notFound(w, "app not found")
			return
		}

		// Validate target bundle.
		b, err := srv.DB.GetBundle(body.BundleID)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}
		if b == nil || b.AppID != app.ID {
			notFound(w, "bundle not found")
			return
		}
		if b.Status != "ready" {
			badRequest(w, "bundle is not ready (status: "+b.Status+")")
			return
		}
		if app.ActiveBundle != nil && *app.ActiveBundle == body.BundleID {
			badRequest(w, "bundle is already active")
			return
		}

		slog.Info("rolling back app", //nolint:gosec // G706: slog structured logging handles this
			"app_id", app.ID, "name", app.Name,
			"target_bundle", body.BundleID, "caller", caller.Sub)

		// Capture previous bundle before switching.
		previousBundle := app.ActiveBundle

		// Drain and stop running workers.
		ops.StopAppSync(srv, app.ID)

		// Switch active bundle and record deployment tracking.
		if err := srv.DB.SetActiveBundle(app.ID, body.BundleID); err != nil {
			serverError(w, "set active bundle: "+err.Error())
			return
		}
		if err := srv.DB.SetBundleDeployed(body.BundleID, caller.Sub); err != nil {
			slog.Warn("rollback: failed to update deployment tracking", //nolint:gosec // G706: slog structured logging handles this
				"bundle_id", body.BundleID, "error", err)
		}

		// Re-read app to get updated state.
		app, err = srv.DB.GetApp(app.ID)
		if err != nil || app == nil {
			serverError(w, "get app after rollback")
			return
		}

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionAppRollback, app.ID,
				map[string]any{
					"bundle_id":          body.BundleID,
					"previous_bundle_id": stringOrNil(previousBundle),
				}))
		}

		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Trigger", `{"bundleRolledBack":"","showToast":{"message":"Rolled back","type":"success"}}`)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(appResponseV2(app, srv))
	}
}

// RestoreApp restores a soft-deleted application.
//
//	@Summary		Restore deleted app
//	@Description	Restore a soft-deleted app. Only admins and the original owner can restore.
//	@Tags			apps
//	@Produce		json
//	@Param			id	path		string	true	"App ID (UUID) or name"
//	@Success		200	{object}	AppResponse
//	@Failure		404	{object}	errorResponse
//	@Failure		409	{object}	errorResponse
//	@Failure		500	{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/restore [post]
func RestoreApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		if caller == nil {
			forbidden(w, "insufficient permissions")
			return
		}

		// Look up the app including deleted — supports both UUID and name.
		app, err := resolveAppIncludeDeleted(srv.DB, id)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}
		if app == nil || app.DeletedAt == nil {
			notFound(w, "deleted app not found")
			return
		}

		// Only admins and the original owner can restore.
		if !caller.Role.CanViewAllApps() && app.Owner != caller.Sub {
			notFound(w, "deleted app not found")
			return
		}

		if err := srv.DB.RestoreApp(app.ID); err != nil {
			if db.IsUniqueConstraintError(err) {
				conflict(w, "another app already uses the name "+app.Name)
				return
			}
			serverError(w, "restore app: "+err.Error())
			return
		}

		app, err = srv.DB.GetApp(app.ID)
		if err != nil || app == nil {
			serverError(w, "get app after restore")
			return
		}

		slog.Info("app restored",
			"app_id", app.ID, "name", app.Name, "caller", caller.Sub)

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionAppRestore, app.ID,
				map[string]any{"name": app.Name}))
		}

		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Trigger", `{"showToast":{"message":"App restored","type":"success"}}`)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(appResponseV2(app, srv))
	}
}

func stringOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// --- App lifecycle ---

type startResponse struct {
	WorkerID string `json:"worker_id"`
	Status   string `json:"status"`
}

func StartApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		app, relation, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}

		if !relation.CanStartStop() {
			notFound(w, "app not found")
			return
		}

		// Already running — return existing worker
		workerIDs := srv.Workers.ForApp(app.ID)
		if len(workerIDs) > 0 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(startResponse{
				WorkerID: workerIDs[0],
				Status:   "running",
			})
			return
		}

		// Must have an active bundle
		if app.ActiveBundle == nil {
			conflict(w, "app has no active bundle — upload and build a bundle first")
			return
		}

		// Check global worker limit
		if srv.Workers.Count() >= srv.Config.Proxy.MaxWorkers {
			serviceUnavailable(w, "max workers reached")
			return
		}

		// Build WorkerSpec
		workerID := uuid.New().String()
		hostPaths := bundle.NewBundlePaths(
			srv.Config.Storage.BundleServerPath, app.ID, *app.ActiveBundle,
		)

		labels := map[string]string{
			"dev.blockyard/managed":   "true",
			"dev.blockyard/app-id":    app.ID,
			"dev.blockyard/worker-id": workerID,
			"dev.blockyard/role":      "worker",
		}

		var rVersion string
		if m, err := manifest.Read(filepath.Join(hostPaths.Unpacked, "manifest.json")); err == nil {
			rVersion = m.RVersion
		}

		spec := backend.WorkerSpec{
			AppID:       app.ID,
			WorkerID:    workerID,
			Image:       server.AppImage(app, srv.Config.Docker.Image),
			Cmd: []string{"R", "-e",
				fmt.Sprintf("shiny::runApp('%s', port = as.integer(Sys.getenv('SHINY_PORT')), host = Sys.getenv('SHINY_HOST', unset = '0.0.0.0'))",
					srv.Config.Storage.BundleWorkerPath)},
			BundlePath:  hostPaths.Unpacked,
			LibraryPath: hostPaths.Library,
			WorkerMount: srv.Config.Storage.BundleWorkerPath,
			ShinyPort:   srv.Config.Docker.ShinyPort,
			RVersion:    rVersion,
			MemoryLimit: stringOrEmpty(app.MemoryLimit),
			CPULimit:    floatOrZero(app.CPULimit),
			Labels:      labels,
			Env:         server.WorkerEnv(srv),
			Runtime:     server.AppRuntime(app, srv.Config.Docker),
		}

		// Resolve per-app data mounts.
		appMounts, _ := srv.DB.ListAppDataMounts(app.ID)
		if len(appMounts) > 0 {
			if resolved, err := mount.Resolve(appMounts, srv.Config.Storage.DataMounts); err == nil {
				spec.DataMounts = resolved
			}
		}

		slog.Info("starting app via API",
			"app_id", app.ID, "name", app.Name, "worker_id", workerID)

		// Use a dedicated context so client disconnects don't cancel
		// Docker operations mid-flight.
		spawnCtx, spawnCancel := context.WithTimeout(context.Background(),
			srv.Config.Proxy.WorkerStartTimeout.Duration)
		defer spawnCancel()

		if err := srv.Backend.Spawn(spawnCtx, spec); err != nil {
			serverError(w, "spawn worker: "+err.Error())
			return
		}

		srv.Workers.Set(workerID, server.ActiveWorker{AppID: app.ID, BundleID: *app.ActiveBundle, StartedAt: time.Now()})

		addr, err := srv.Backend.Addr(spawnCtx, workerID)
		if err != nil {
			slog.Warn("failed to get worker address", "worker_id", workerID, "error", err)
		} else {
			srv.Registry.Set(workerID, addr)
		}

		// Start log capture before health polling so startup output is visible.
		ops.SpawnLogCapture(context.Background(), srv, workerID, app.ID)

		// Wait for the worker to become healthy before reporting success.
		if err := pollWorkerHealthy(spawnCtx, srv, workerID); err != nil {
			srv.Workers.Delete(workerID)
			srv.Registry.Delete(workerID)
			srv.Backend.Stop(context.Background(), workerID) //nolint:errcheck // best-effort cleanup
			slog.Error("worker failed to start", "app_id", app.ID, "worker_id", workerID, "error", err)
			serviceUnavailable(w, "worker failed to start")
			return
		}

		srv.Metrics.WorkersSpawned.Inc()
		srv.Metrics.WorkersActive.Inc()

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionAppStart, app.ID, nil))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(startResponse{
			WorkerID: workerID,
			Status:   "running",
		})
	}
}

func StopApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		app, relation, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}

		if !relation.CanStartStop() {
			notFound(w, "app not found")
			return
		}

		slog.Info("stopping app",
			"app_id", app.ID, "name", app.Name, "caller", caller.Sub)

		// Mark draining so no new sessions are routed.
		workerIDs := srv.Workers.MarkDraining(app.ID)
		if len(workerIDs) == 0 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"stopped_workers": 0,
			})
			return
		}

		// Create task for drain tracking.
		taskID := uuid.New().String()
		sender := srv.Tasks.Create(taskID, app.ID)
		sender.Write(fmt.Sprintf("draining %d workers", len(workerIDs)))

		// Drain in background — caller polls GET /tasks/{taskID}/logs.
		go drainWorkers(srv, app.ID, workerIDs, sender) //nolint:gosec // G118: intentional background drain, outlives request

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionAppStop, app.ID,
				map[string]any{"worker_count": len(workerIDs)}))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"task_id":      taskID,
			"worker_count": len(workerIDs),
		})
	}
}

// drainWorkers waits for sessions to end, then evicts workers.
// Writes progress to the task sender so operators can stream it
// via GET /tasks/{taskID}/logs.
func drainWorkers(srv *server.Server, appID string, workerIDs []string, sender task.Sender) {
	defer sender.Complete(task.Completed)

	deadline := time.Now().Add(srv.Config.Server.ShutdownTimeout.Duration)

	for {
		remaining := srv.Sessions.CountForWorkers(workerIDs)
		if remaining == 0 {
			sender.Write("all sessions ended")
			break
		}
		if time.Now().After(deadline) {
			sender.Write(fmt.Sprintf("drain timeout reached, %d sessions remaining — forcing stop", remaining))
			break
		}
		time.Sleep(time.Second)
	}

	for _, wid := range workerIDs {
		ops.EvictWorker(context.Background(), srv, wid)
		sender.Write(fmt.Sprintf("stopped worker %s", wid))
	}
	sender.Write(fmt.Sprintf("stopped %d workers", len(workerIDs)))
}


// AppLogs streams logs from the LogStore for a specific worker.
//
//	@Summary		Stream app logs
//	@Description	Stream worker logs for an app. Returns buffered output immediately; streams live lines unless stream=false.
//	@Tags			apps
//	@Produce		plain
//	@Param			id			path	string	true	"App ID (UUID) or name"
//	@Param			worker_id	query	string	true	"Worker ID"
//	@Param			stream		query	string	false	"Set to 'false' to return buffered logs only"
//	@Success		200			"Log output (text/plain, chunked)"
//	@Failure		400			{object}	errorResponse
//	@Failure		404			{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/logs [get]
func AppLogs(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		app, relation, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}
		if !relation.CanDeploy() {
			notFound(w, "app not found")
			return
		}

		workerID := r.URL.Query().Get("worker_id")
		if workerID == "" {
			badRequest(w, "worker_id query parameter is required")
			return
		}

		// Verify the worker belongs to this app — check both live workers
		// and the logstore (for dead workers with historical logs).
		worker, workerExists := srv.Workers.Get(workerID)
		if workerExists && worker.AppID != app.ID {
			notFound(w, "worker not found for app")
			return
		}
		if !workerExists {
			// Check logstore for dead workers.
			logAppID := srv.LogStore.WorkerAppID(workerID)
			if logAppID == "" || logAppID != app.ID {
				notFound(w, "worker not found for app")
				return
			}
		}

		snapshot, live, ok := srv.LogStore.Subscribe(workerID)
		if !ok {
			notFound(w, "no logs for worker "+workerID)
			return
		}
		ended := srv.LogStore.IsEnded(workerID)
		streamParam := r.URL.Query().Get("stream")
		noStream := streamParam == "false"

		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Transfer-Encoding", "chunked")

		flusher, canFlush := w.(http.Flusher)

		// Write buffered lines
		for _, line := range snapshot {
			fmt.Fprintf(w, "%s\n", line) //nolint:gosec // G705: text/plain SSE stream, not HTML
		}
		if canFlush {
			flusher.Flush()
		}

		// If worker already exited or stream=false, return buffer only
		if ended || noStream {
			return
		}

		// Stream live lines
		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case line, ok := <-live:
				if !ok {
					return // stream ended
				}
				fmt.Fprintf(w, "%s\n", line)
				if canFlush {
					flusher.Flush()
				}
			}
		}
	}
}

func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func floatOrZero(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// parseUpdateAppForm parses form-encoded data into an updateAppRequest.
func parseUpdateAppForm(r *http.Request) updateAppRequest {
	var body updateAppRequest
	if v := r.FormValue("name"); v != "" {
		body.Name = &v
	}
	if v := r.FormValue("title"); v != "" {
		body.Title = &v
	}
	if v := r.FormValue("description"); v != "" {
		body.Description = &v
	}
	if v := r.FormValue("access_type"); v != "" {
		body.AccessType = &v
	}
	if v := r.FormValue("refresh_schedule"); r.Form.Has("refresh_schedule") {
		body.RefreshSchedule = &v
	}
	if v := r.FormValue("memory_limit"); v != "" {
		body.MemoryLimit = &v
	}
	if v := r.FormValue("max_workers_per_app"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			body.MaxWorkersPerApp = &n
		}
	}
	if v := r.FormValue("max_sessions_per_worker"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			body.MaxSessionsPerWorker = &n
		}
	}
	if v := r.FormValue("pre_warmed_sessions"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			body.PreWarmedSessions = &n
		}
	}
	if v := r.FormValue("cpu_limit"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			body.CPULimit = &f
		}
	}
	if v := r.FormValue("image"); r.Form.Has("image") {
		body.Image = &v
	}
	if v := r.FormValue("runtime"); r.Form.Has("runtime") {
		body.Runtime = &v
	}
	// data_mounts not supported via form encoding — use JSON API.
	return body
}

// isValidCron validates a standard five-field cron expression.
func isValidCron(expr string) bool {
	fields := strings.Fields(expr)
	return len(fields) == 5
}

// pollWorkerHealthy polls the backend health check with exponential backoff
// until the worker is healthy or the context expires (worker_start_timeout).
func pollWorkerHealthy(ctx context.Context, srv *server.Server, workerID string) error {
	interval := 100 * time.Millisecond
	maxInterval := 2 * time.Second

	for {
		if srv.Backend.HealthCheck(ctx, workerID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("worker did not become healthy within timeout")
		case <-time.After(interval):
		}
		interval = min(interval*2, maxInterval)
	}
}
