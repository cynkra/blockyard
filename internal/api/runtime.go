package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/server"
)

// runtimeWorker is the JSON shape for a worker in the runtime response.
type runtimeWorker struct {
	ID        string              `json:"id"`
	BundleID  string              `json:"bundle_id"`
	Status    string              `json:"status"`
	StartedAt string              `json:"started_at"`
	EndedAt   *string             `json:"ended_at,omitempty"`
	IdleSince *string             `json:"idle_since,omitempty"`
	Stats     *runtimeWorkerStats `json:"stats"`
	Sessions  []runtimeSession    `json:"sessions"`
}

type runtimeWorkerStats struct {
	CPUPercent       float64 `json:"cpu_percent"`
	MemoryUsageBytes uint64  `json:"memory_usage_bytes"`
	MemoryLimitBytes uint64  `json:"memory_limit_bytes"`
}

type runtimeSession struct {
	ID              string  `json:"id"`
	UserSub         *string `json:"user_sub"`
	UserDisplayName string  `json:"user_display_name,omitempty"`
	StartedAt       string  `json:"started_at"`
}

type runtimeResponse struct {
	Workers        []runtimeWorker `json:"workers"`
	ActiveSessions int             `json:"active_sessions"`
	TotalViews     int             `json:"total_views"`
	RecentViews    int             `json:"recent_views"`
	UniqueVisitors int             `json:"unique_visitors"`
	LastDeployedAt *string         `json:"last_deployed_at"`
}

// GetAppRuntime returns live operational data for an app (collaborator+).
//
//	@Summary		Get app runtime
//	@Description	Returns live workers, sessions, container stats, and activity metrics for an app.
//	@Tags			apps
//	@Produce		json
//	@Param			id	path		string	true	"App ID (UUID) or name"
//	@Success		200	{object}	runtimeResponse
//	@Failure		404	{object}	errorResponse
//	@Failure		500	{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/runtime [get]
func GetAppRuntime(srv *server.Server) http.HandlerFunc {
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

		resp := buildRuntimeResponse(srv, app)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func buildRuntimeResponse(srv *server.Server, app *db.AppRow) runtimeResponse {
	var workers []runtimeWorker

	// Live workers from the worker map.
	for _, wid := range srv.Workers.ForApp(app.ID) {
		aw, ok := srv.Workers.Get(wid)
		if !ok {
			continue
		}

		status := "active"
		if aw.Draining {
			status = "draining"
		}

		rw := runtimeWorker{
			ID:        wid,
			BundleID:  aw.BundleID,
			Status:    status,
			StartedAt: aw.StartedAt.UTC().Format(time.RFC3339),
			Sessions:  []runtimeSession{},
		}

		if !aw.IdleSince.IsZero() {
			idle := aw.IdleSince.UTC().Format(time.RFC3339)
			rw.IdleSince = &idle
		}

		// Container stats (best-effort).
		stats, err := srv.Backend.ContainerStats(r_context(), wid)
		if err == nil && stats != nil {
			rw.Stats = &runtimeWorkerStats{
				CPUPercent:       stats.CPUPercent,
				MemoryUsageBytes: stats.MemoryUsageBytes,
				MemoryLimitBytes: stats.MemoryLimitBytes,
			}
		}

		// Active sessions for this worker from the session store.
		for sid, entry := range srv.Sessions.EntriesForWorker(wid) {
			rs := runtimeSession{
				ID:        sid,
				StartedAt: entry.LastAccess.UTC().Format(time.RFC3339),
			}
			if entry.UserSub != "" {
				rs.UserSub = &entry.UserSub
				// Look up display name.
				if u, err := srv.DB.GetUser(entry.UserSub); err == nil && u != nil {
					rs.UserDisplayName = u.Name
				}
			}
			rw.Sessions = append(rw.Sessions, rs)
		}

		workers = append(workers, rw)
	}

	// Dead workers from the logstore.
	for _, info := range srv.LogStore.WorkerIDsByApp(app.ID) {
		if !info.Ended {
			continue // live worker already included above
		}
		endedAt := info.EndedAt.UTC().Format(time.RFC3339)
		workers = append(workers, runtimeWorker{
			ID:        info.ID,
			Status:    "ended",
			StartedAt: info.StartedAt.UTC().Format(time.RFC3339),
			EndedAt:   &endedAt,
			Sessions:  []runtimeSession{},
		})
	}

	// Activity metrics from DB.
	totalViews, _ := srv.DB.CountSessions(app.ID)
	recentViews, _ := srv.DB.CountRecentSessions(app.ID, time.Now().AddDate(0, 0, -7))
	uniqueVisitors, _ := srv.DB.CountUniqueVisitors(app.ID)

	// Active session count.
	activeSessions := 0
	for _, rw := range workers {
		activeSessions += len(rw.Sessions)
	}

	// Last deployed at from active bundle.
	var lastDeployedAt *string
	if app.ActiveBundle != nil {
		if b, err := srv.DB.GetBundle(*app.ActiveBundle); err == nil && b != nil {
			lastDeployedAt = b.DeployedAt
		}
	}

	if workers == nil {
		workers = []runtimeWorker{}
	}

	return runtimeResponse{
		Workers:        workers,
		ActiveSessions: activeSessions,
		TotalViews:     totalViews,
		RecentViews:    recentViews,
		UniqueVisitors: uniqueVisitors,
		LastDeployedAt: lastDeployedAt,
	}
}

// r_context returns a background context for non-critical operations
// like fetching container stats.
func r_context() context.Context { return context.Background() }

// --- Deployments API ---

type deploymentsResponse struct {
	Deployments []db.DeploymentRow `json:"deployments"`
	Total       int                `json:"total"`
	Page        int                `json:"page"`
	PerPage     int                `json:"per_page"`
}

// ListDeployments returns GET /api/v1/deployments — cross-app deployment listing.
//
//	@Summary		List deployments
//	@Description	List bundle deployments across all apps. Supports search, status filter, and pagination.
//	@Tags			deployments
//	@Produce		json
//	@Param			search		query		string	false	"Search by app name"
//	@Param			status		query		string	false	"Filter by status (e.g. ready, pending, failed)"
//	@Param			page		query		int		false	"Page number"	default(1)
//	@Param			per_page	query		int		false	"Items per page (1-100)"	default(25)
//	@Success		200			{object}	deploymentsResponse
//	@Failure		403			{object}	errorResponse
//	@Failure		500			{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/deployments [get]
func ListDeployments(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil {
			forbidden(w, "authentication required")
			return
		}

		opts := db.DeploymentListOpts{
			CallerSub:  caller.Sub,
			CallerRole: caller.Role.String(),
			Search:     r.URL.Query().Get("search"),
			Status:     r.URL.Query().Get("status"),
			Page:       parseIntOr(r.URL.Query().Get("page"), 1),
			PerPage:    clamp(parseIntOr(r.URL.Query().Get("per_page"), 25), 1, 100),
		}

		deployments, total, err := srv.DB.ListDeployments(opts)
		if err != nil {
			serverError(w, "list deployments: "+err.Error())
			return
		}

		if deployments == nil {
			deployments = []db.DeploymentRow{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(deploymentsResponse{
			Deployments: deployments,
			Total:       total,
			Page:        opts.Page,
			PerPage:     opts.PerPage,
		})
	}
}

// --- Sessions API ---

// ListAppSessions returns GET /api/v1/apps/{id}/sessions — list sessions for an app.
//
//	@Summary		List app sessions
//	@Description	List sessions for an app. Supports filtering by user and status.
//	@Tags			apps
//	@Produce		json
//	@Param			id		path		string	true	"App ID (UUID) or name"
//	@Param			user	query		string	false	"Filter by user sub"
//	@Param			status	query		string	false	"Filter by status (active, ended)"
//	@Param			limit	query		int		false	"Max results (1-200)"	default(50)
//	@Success		200		{object}	sessionListResponse
//	@Failure		404		{object}	errorResponse
//	@Failure		500		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/sessions [get]
func ListAppSessions(srv *server.Server) http.HandlerFunc {
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

		opts := db.SessionListOpts{
			UserSub: r.URL.Query().Get("user"),
			Status:  r.URL.Query().Get("status"),
			Limit:   clamp(parseIntOr(r.URL.Query().Get("limit"), 50), 1, 200),
		}

		sessions, err := srv.DB.ListSessions(app.ID, opts)
		if err != nil {
			serverError(w, "list sessions: "+err.Error())
			return
		}

		if sessions == nil {
			sessions = []db.SessionRow{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"sessions": sessions,
		})
	}
}

// --- Per-app tags listing ---

// ListAppTags returns GET /api/v1/apps/{id}/tags — list tags for an app.
//
//	@Summary		List app tags
//	@Description	List all tags attached to an app.
//	@Tags			tags
//	@Produce		json
//	@Param			id	path		string	true	"App ID (UUID) or name"
//	@Success		200	{object}	appTagListResponse
//	@Failure		404	{object}	errorResponse
//	@Failure		500	{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/tags [get]
func ListAppTags(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		app, _, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}

		tags, err := srv.DB.ListAppTags(app.ID)
		if err != nil {
			serverError(w, "list app tags: "+err.Error())
			return
		}

		resp := make([]tagResponse, 0, len(tags))
		for _, t := range tags {
			resp = append(resp, tagResp(&t))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"tags": resp,
		})
	}
}

// --- User profile API ---

// GetCurrentUser returns GET /api/v1/users/me — caller's own profile.
//
//	@Summary		Get current user
//	@Description	Returns the authenticated caller's profile (sub, email, name, role).
//	@Tags			users
//	@Produce		json
//	@Success		200	{object}	currentUserResponse
//	@Failure		401	{object}	errorResponse
//	@Failure		500	{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/users/me [get]
func GetCurrentUser(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}

		user, err := srv.DB.GetUser(caller.Sub)
		if err != nil {
			serverError(w, "get user: "+err.Error())
			return
		}
		if user == nil {
			// Caller is authenticated but has no DB row (e.g. PAT user not yet synced).
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"sub":  caller.Sub,
				"name": caller.Name,
				"role": caller.Role.String(),
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"sub":   user.Sub,
			"email": user.Email,
			"name":  user.Name,
			"role":  user.Role,
		})
	}
}

// --- Enable/Disable API ---

// EnableApp handles POST /api/v1/apps/{id}/enable.
//
//	@Summary		Enable app
//	@Description	Enable an app, allowing it to accept traffic and start workers.
//	@Tags			apps
//	@Produce		json
//	@Param			id	path		string	true	"App ID (UUID) or name"
//	@Success		200	{object}	appResponseV2JSON
//	@Failure		404	{object}	errorResponse
//	@Failure		500	{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/enable [post]
func EnableApp(srv *server.Server) http.HandlerFunc {
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

		if err := srv.DB.SetAppEnabled(app.ID, true); err != nil {
			serverError(w, "enable app: "+err.Error())
			return
		}

		app, err := srv.DB.GetApp(app.ID)
		if err != nil || app == nil {
			serverError(w, "get app after enable")
			return
		}

		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Trigger", "appEnabled")
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(appResponseV2(app, srv))
	}
}

// DisableApp handles POST /api/v1/apps/{id}/disable.
//
//	@Summary		Disable app
//	@Description	Disable an app, draining active sessions and stopping all workers.
//	@Tags			apps
//	@Produce		json
//	@Param			id	path		string	true	"App ID (UUID) or name"
//	@Success		200	{object}	appResponseV2JSON
//	@Failure		404	{object}	errorResponse
//	@Failure		500	{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/disable [post]
func DisableApp(srv *server.Server) http.HandlerFunc {
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

		slog.Info("disabling app",
			"app_id", app.ID, "name", app.Name, "caller", caller.Sub)

		if err := srv.DB.SetAppEnabled(app.ID, false); err != nil {
			serverError(w, "disable app: "+err.Error())
			return
		}

		// End all sessions for the app in the DB.
		if err := srv.DB.EndAppSessions(app.ID); err != nil {
			slog.Warn("disable: failed to end app sessions",
				"app_id", app.ID, "error", err)
		}

		// Drain workers (same logic as the old StopApp).
		workerIDs := srv.Workers.MarkDraining(app.ID)
		if len(workerIDs) > 0 {
			go func() {
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
			}()
		}

		app, err := srv.DB.GetApp(app.ID)
		if err != nil || app == nil {
			serverError(w, "get app after disable")
			return
		}

		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Trigger", "appDisabled")
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(appResponseV2(app, srv))
	}
}

// --- Hard Delete (Purge) API ---

// HardDeleteApp handles DELETE /api/v1/apps/{id}?purge=true — admin-only permanent deletion.
func HardDeleteApp(srv *server.Server, purge bool, w http.ResponseWriter, r *http.Request,
	caller *auth.CallerIdentity, id string) {
	if !caller.Role.CanViewAllApps() {
		forbidden(w, "admin only")
		return
	}

	// Use include-deleted lookup since purge targets soft-deleted apps.
	app, err := resolveAppIncludeDeleted(srv.DB, id)
	if err != nil {
		serverError(w, "db error: "+err.Error())
		return
	}
	if app == nil {
		notFound(w, "app not found")
		return
	}

	if app.DeletedAt == nil {
		conflict(w, "app must be soft-deleted before purging")
		return
	}

	// Stop any remaining workers.
	ops.StopAppSync(srv, app.ID)

	// Purge all data.
	if err := srv.DB.PurgeApp(app.ID); err != nil {
		serverError(w, "purge app: "+err.Error())
		return
	}

	// Clean up files.
	appDir := filepath.Join(srv.Config.Storage.BundleServerPath, app.ID)
	_ = os.RemoveAll(appDir)

	slog.Info("purged app", "app_id", app.ID, "name", app.Name, "caller", caller.Sub)

	w.WriteHeader(http.StatusNoContent)
}

// resolveAppIncludeDeleted looks up an app by UUID or name, including soft-deleted.
func resolveAppIncludeDeleted(database *db.DB, id string) (*db.AppRow, error) {
	app, err := database.GetAppIncludeDeleted(id)
	if err != nil {
		return nil, err
	}
	if app != nil {
		return app, nil
	}
	return database.GetAppByNameIncludeDeleted(id)
}

// --- Updated app response (v2 shape without workers) ---

// appResponseV2 builds the new app response shape (no workers, with status/enabled/tags/relation).
func appResponseV2(app *db.AppRow, srv *server.Server) map[string]any {
	status := "stopped"
	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) > 0 {
		allDraining := true
		for _, wid := range workerIDs {
			w, ok := srv.Workers.Get(wid)
			if ok && !w.Draining {
				allDraining = false
				break
			}
		}
		if allDraining {
			status = "stopping"
		} else {
			status = "running"
		}
	}

	tags, _ := srv.DB.ListAppTags(app.ID)
	tagNames := make([]string, len(tags))
	for i, t := range tags {
		tagNames[i] = t.Name
	}

	return map[string]any{
		"id":                     app.ID,
		"name":                   app.Name,
		"owner":                  app.Owner,
		"access_type":            app.AccessType,
		"active_bundle":          app.ActiveBundle,
		"max_workers_per_app":    app.MaxWorkersPerApp,
		"max_sessions_per_worker": app.MaxSessionsPerWorker,
		"memory_limit":           app.MemoryLimit,
		"cpu_limit":              app.CPULimit,
		"title":                  app.Title,
		"description":            app.Description,
		"pre_warmed_seats":       app.PreWarmedSeats,
		"enabled":                app.Enabled,
		"refresh_schedule":       app.RefreshSchedule,
		"created_at":             app.CreatedAt,
		"updated_at":             app.UpdatedAt,
		"status":                 status,
		"tags":                   tagNames,
	}
}

// appResponseV2WithRelation adds the relation field to the v2 response.
func appResponseV2WithRelation(app *db.AppRow, srv *server.Server, relation string) map[string]any {
	resp := appResponseV2(app, srv)
	resp["relation"] = relation
	return resp
}

// computeStatus computes the app status from the worker map.
func computeStatus(srv *server.Server, appID string) string {
	workerIDs := srv.Workers.ForApp(appID)
	if len(workerIDs) == 0 {
		return "stopped"
	}
	for _, wid := range workerIDs {
		w, ok := srv.Workers.Get(wid)
		if ok && !w.Draining {
			return "running"
		}
	}
	return "stopping"
}

// --- Consolidated list response ---

type appListItem struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Owner       string   `json:"owner"`
	AccessType  string   `json:"access_type"`
	ActiveBundle *string `json:"active_bundle"`
	Title       *string  `json:"title"`
	Description *string  `json:"description"`
	Enabled     bool     `json:"enabled"`
	Status      string   `json:"status"`
	Relation    string   `json:"relation"`
	Tags        []string `json:"tags"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

type appListResponse struct {
	Apps    []appListItem `json:"apps"`
	Total   int           `json:"total"`
	Page    int           `json:"page"`
	PerPage int           `json:"per_page"`
}

// ListAppsV2 handles GET /api/v1/apps — consolidated with catalog, paginated.
//
//	@Summary		List apps
//	@Description	List apps with RBAC filtering, search, tag filter, and pagination. Use ?deleted=true for soft-deleted apps (admin only).
//	@Tags			apps
//	@Produce		json
//	@Param			search		query		string	false	"Search by name/title"
//	@Param			tag			query		string	false	"Filter by tag name"
//	@Param			deleted		query		bool	false	"Show soft-deleted apps (admin only)"
//	@Param			page		query		int		false	"Page number"	default(1)
//	@Param			per_page	query		int		false	"Items per page (1-100)"	default(25)
//	@Success		200			{object}	appListResponse
//	@Failure		403			{object}	errorResponse
//	@Failure		500			{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps [get]
func ListAppsV2(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil {
			forbidden(w, "authentication required")
			return
		}

		// ?deleted=true — admin-only, returns soft-deleted apps
		if r.URL.Query().Get("deleted") == "true" {
			if !caller.Role.CanViewAllApps() {
				forbidden(w, "admin only")
				return
			}
			apps, err := srv.DB.ListDeletedApps()
			if err != nil {
				serverError(w, "db error: "+err.Error())
				return
			}
			responses := make([]map[string]any, len(apps))
			for i, app := range apps {
				responses[i] = appResponseV2(&app, srv)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(responses)
			return
		}

		params := db.CatalogParams{
			CallerSub:  caller.Sub,
			CallerRole: caller.Role.String(),
			Tag:        r.URL.Query().Get("tag"),
			Search:     r.URL.Query().Get("search"),
			Page:       parseIntOr(r.URL.Query().Get("page"), 1),
			PerPage:    clamp(parseIntOr(r.URL.Query().Get("per_page"), 25), 1, 100),
		}

		rows, total, err := srv.DB.ListCatalogWithRelation(params)
		if err != nil {
			serverError(w, "list apps: "+err.Error())
			return
		}

		items := make([]appListItem, 0, len(rows))
		for _, row := range rows {
			var tagNames []string
			if row.Tags != "" {
				tagNames = strings.Split(row.Tags, ",")
			}
			if tagNames == nil {
				tagNames = []string{}
			}

			items = append(items, appListItem{
				ID:           row.ID,
				Name:         row.Name,
				Owner:        row.Owner,
				AccessType:   row.AccessType,
				ActiveBundle: row.ActiveBundle,
				Title:        row.Title,
				Description:  row.Description,
				Enabled:      row.Enabled,
				Status:       computeStatus(srv, row.ID),
				Relation:     row.Relation,
				Tags:         tagNames,
				CreatedAt:    row.CreatedAt,
				UpdatedAt:    row.UpdatedAt,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(appListResponse{
			Apps:    items,
			Total:   total,
			Page:    params.Page,
			PerPage: params.PerPage,
		})
	}
}
