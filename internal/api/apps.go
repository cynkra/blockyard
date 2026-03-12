package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/server"
)

// AppResponse wraps an AppRow with a derived runtime status.
type AppResponse struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	ActiveBundle         *string  `json:"active_bundle"`
	MaxWorkersPerApp     *int     `json:"max_workers_per_app"`
	MaxSessionsPerWorker int      `json:"max_sessions_per_worker"`
	MemoryLimit          *string  `json:"memory_limit"`
	CPULimit             *float64 `json:"cpu_limit"`
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
		ActiveBundle:         app.ActiveBundle,
		MaxWorkersPerApp:     app.MaxWorkersPerApp,
		MaxSessionsPerWorker: app.MaxSessionsPerWorker,
		MemoryLimit:          app.MemoryLimit,
		CPULimit:             app.CPULimit,
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

		app, err := srv.DB.CreateApp(body.Name)
		if err != nil {
			serverError(w, "create app: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(appResponse(app, srv.Workers))
	}
}

func ListApps(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apps, err := srv.DB.ListApps()
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
		id := chi.URLParam(r, "id")

		app, err := resolveApp(srv.DB, id)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}
		if app == nil {
			notFound(w, "app "+id+" not found")
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
}

func UpdateApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		var body updateAppRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid JSON body")
			return
		}

		// v0: max_sessions_per_worker is locked to 1
		if body.MaxSessionsPerWorker != nil && *body.MaxSessionsPerWorker != 1 {
			badRequest(w, "max_sessions_per_worker must be 1 in this version")
			return
		}

		app, err := resolveApp(srv.DB, id)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}
		if app == nil {
			notFound(w, "app "+id+" not found")
			return
		}

		update := db.AppUpdate{
			MaxWorkersPerApp:     body.MaxWorkersPerApp,
			MaxSessionsPerWorker: body.MaxSessionsPerWorker,
			MemoryLimit:          body.MemoryLimit,
			CPULimit:             body.CPULimit,
		}
		app, err = srv.DB.UpdateApp(app.ID, update)
		if err != nil {
			serverError(w, "update app: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(appResponse(app, srv.Workers))
	}
}

func DeleteApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		app, err := resolveApp(srv.DB, id)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}
		if app == nil {
			notFound(w, "app "+id+" not found")
			return
		}

		// 1. Stop all workers for this app
		stopAppWorkers(srv, app.ID)

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
		id := chi.URLParam(r, "id")

		app, err := resolveApp(srv.DB, id)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}
		if app == nil {
			notFound(w, "app "+id+" not found")
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
		paths := bundle.NewBundlePaths(
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
			BundlePath:  paths.Unpacked,
			LibraryPath: paths.Library,
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

		ops.SpawnLogCapture(r.Context(), srv, workerID, app.ID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(startResponse{
			WorkerID: workerID,
			Status:   "running",
		})
	}
}

func StopApp(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		app, err := resolveApp(srv.DB, id)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}
		if app == nil {
			notFound(w, "app "+id+" not found")
			return
		}

		stopped := stopAppWorkers(srv, app.ID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":          "stopped",
			"workers_stopped": stopped,
		})
	}
}

// stopAppWorkers stops all workers belonging to the given app. Returns
// the count of workers stopped.
func stopAppWorkers(srv *server.Server, appID string) int {
	workerIDs := srv.Workers.ForApp(appID)
	for _, wid := range workerIDs {
		ops.EvictWorker(context.Background(), srv, wid)
	}
	return len(workerIDs)
}

// AppLogs streams logs from the LogStore for a specific worker.
func AppLogs(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		app, err := resolveApp(srv.DB, id)
		if err != nil {
			serverError(w, "db error: "+err.Error())
			return
		}
		if app == nil {
			notFound(w, "app "+id+" not found")
			return
		}

		workerID := r.URL.Query().Get("worker_id")
		if workerID == "" {
			badRequest(w, "worker_id query parameter is required")
			return
		}

		snapshot, live, ok := srv.LogStore.Subscribe(workerID)
		if !ok {
			notFound(w, "no logs for worker "+workerID)
			return
		}
		ended := srv.LogStore.IsEnded(workerID)

		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Content-Type-Options", "nosniff")

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
