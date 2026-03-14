package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/authz"
	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/telemetry"
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
	CreatedAt            string   `json:"created_at"`
	UpdatedAt            string   `json:"updated_at"`
	Status               string   `json:"status"`
	Workers              []string `json:"workers"`
}

func appResponse(app *db.AppRow, workers *server.WorkerMap) AppResponse {
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
		CreatedAt:            app.CreatedAt,
		UpdatedAt:            app.UpdatedAt,
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
	return database.GetAppByName(id)
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
	if len(name) == 0 || len(name) > 63 {
		return fmt.Errorf("name must be 1-63 characters")
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return fmt.Errorf("name must contain only lowercase letters, digits, and hyphens")
		}
	}
	if name[0] < 'a' || name[0] > 'z' {
		return fmt.Errorf("name must start with a lowercase letter")
	}
	if name[len(name)-1] == '-' {
		return fmt.Errorf("name must not end with a hyphen")
	}
	return nil
}

type createAppRequest struct {
	Name string `json:"name"`
}

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

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionAppCreate, app.ID,
				map[string]any{"name": app.Name}))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(appResponse(app, srv.Workers))
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
		if caller.Role.CanViewAllApps() {
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

func GetApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		app, _, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(appResponse(app, srv.Workers))
	}
}

type updateAppRequest struct {
	MaxWorkersPerApp     *int     `json:"max_workers_per_app"`
	MaxSessionsPerWorker *int     `json:"max_sessions_per_worker"`
	MemoryLimit          *string  `json:"memory_limit"`
	CPULimit             *float64 `json:"cpu_limit"`
	AccessType           *string  `json:"access_type"`
	Title                *string  `json:"title"`
	Description          *string  `json:"description"`
}

func UpdateApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		var body updateAppRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid JSON body")
			return
		}

		if body.MaxSessionsPerWorker != nil && *body.MaxSessionsPerWorker < 1 {
			badRequest(w, "max_sessions_per_worker must be >= 1")
			return
		}
		if body.MaxWorkersPerApp != nil && *body.MaxWorkersPerApp < 1 {
			badRequest(w, "max_workers_per_app must be >= 1")
			return
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
			if *body.AccessType != "acl" && *body.AccessType != "public" {
				badRequest(w, "access_type must be 'acl' or 'public'")
				return
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
		}
		app, err := srv.DB.UpdateApp(app.ID, update)
		if err != nil {
			serverError(w, "update app: "+err.Error())
			return
		}

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionAppUpdate, app.ID, nil))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(appResponse(app, srv.Workers))
	}
}

func DeleteApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		app, relation, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}

		if !relation.CanDelete() {
			notFound(w, "app not found")
			return
		}

		// 1. Stop all workers for this app (synchronous for delete).
		stopAppSync(srv, app.ID)

		// 2. Delete bundle files from disk
		bundles, err := srv.DB.ListBundlesByApp(app.ID)
		if err != nil {
			serverError(w, "list bundles: "+err.Error())
			return
		}
		for _, b := range bundles {
			paths := bundle.NewBundlePaths(srv.Config.Storage.BundleServerPath, app.ID, b.ID)
			bundle.DeleteFiles(paths)
		}

		// 3. Clear active_bundle FK before deleting bundles
		if err := srv.DB.ClearActiveBundle(app.ID); err != nil {
			serverError(w, "clear active bundle: "+err.Error())
			return
		}

		// 4. Delete bundle rows
		for _, b := range bundles {
			if _, err := srv.DB.DeleteBundle(b.ID); err != nil {
				slog.Warn("failed to delete bundle row",
					"bundle_id", b.ID, "app_id", app.ID, "error", err)
			}
		}

		// 5. Delete app row
		if _, err := srv.DB.DeleteApp(app.ID); err != nil {
			serverError(w, "delete app: "+err.Error())
			return
		}

		// 6. Remove app directory from disk (best-effort)
		appDir := filepath.Join(srv.Config.Storage.BundleServerPath, app.ID)
		os.RemoveAll(appDir)

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionAppDelete, app.ID,
				map[string]any{"name": app.Name}))
		}

		w.WriteHeader(http.StatusNoContent)
	}
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

		spec := backend.WorkerSpec{
			AppID:       app.ID,
			WorkerID:    workerID,
			Image:       srv.Config.Docker.Image,
			Cmd: []string{"R", "-e",
				fmt.Sprintf("shiny::runApp('%s', port = as.integer(Sys.getenv('SHINY_PORT')))",
					srv.Config.Storage.BundleWorkerPath)},
			BundlePath:  hostPaths.Unpacked,
			LibraryPath: hostPaths.Library,
			WorkerMount: srv.Config.Storage.BundleWorkerPath,
			ShinyPort:   srv.Config.Docker.ShinyPort,
			MemoryLimit: stringOrEmpty(app.MemoryLimit),
			CPULimit:    floatOrZero(app.CPULimit),
			Labels:      labels,
		}

		if err := srv.Backend.Spawn(r.Context(), spec); err != nil {
			serverError(w, "spawn worker: "+err.Error())
			return
		}

		srv.Workers.Set(workerID, server.ActiveWorker{AppID: app.ID})

		addr, err := srv.Backend.Addr(r.Context(), workerID)
		if err != nil {
			slog.Warn("failed to get worker address", "worker_id", workerID, "error", err)
		} else {
			srv.Registry.Set(workerID, addr)
		}

		ops.SpawnLogCapture(context.Background(), srv, workerID, app.ID)
		telemetry.WorkersSpawned.Inc()
		telemetry.WorkersActive.Inc()

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
		sender := srv.Tasks.Create(taskID)
		sender.Write(fmt.Sprintf("draining %d workers", len(workerIDs)))

		// Drain in background — caller polls GET /tasks/{taskID}/logs.
		go drainWorkers(srv, app.ID, workerIDs, sender)

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

// stopAppSync stops all workers for an app synchronously.
// Used by DeleteApp where we must wait for workers to stop before
// deleting the app row. Not suitable for the stop endpoint (use
// the async drainWorkers path instead).
func stopAppSync(srv *server.Server, appID string) {
	workerIDs := srv.Workers.MarkDraining(appID)
	if len(workerIDs) == 0 {
		return
	}

	deadline := time.Now().Add(srv.Config.Server.ShutdownTimeout.Duration)
	for {
		remaining := srv.Sessions.CountForWorkers(workerIDs)
		if remaining == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}

	for _, wid := range workerIDs {
		ops.EvictWorker(context.Background(), srv, wid)
	}
}

// AppLogs streams logs from the LogStore for a specific worker.
func AppLogs(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		app, _, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}

		workerID := r.URL.Query().Get("worker_id")
		if workerID == "" {
			badRequest(w, "worker_id query parameter is required")
			return
		}

		// Verify the worker belongs to this app to prevent cross-app log access.
		worker, workerExists := srv.Workers.Get(workerID)
		if !workerExists || worker.AppID != app.ID {
			notFound(w, "worker not found for app")
			return
		}

		snapshot, live, ok := srv.LogStore.Subscribe(workerID)
		if !ok {
			notFound(w, "no logs for worker "+workerID)
			return
		}
		ended := srv.LogStore.IsEnded(workerID)

		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Transfer-Encoding", "chunked")

		flusher, canFlush := w.(http.Flusher)

		// Write buffered lines
		for _, line := range snapshot {
			fmt.Fprintf(w, "%s\n", line)
		}
		if canFlush {
			flusher.Flush()
		}

		// If worker already exited, return buffer only
		if ended {
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
