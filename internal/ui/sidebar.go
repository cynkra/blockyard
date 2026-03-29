package ui

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/authz"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

// --- Sidebar data types ---

type sidebarData struct {
	App          *db.AppRow
	CanManageACL bool
	OverviewHTML template.HTML
}

type overviewTabData struct {
	App            *db.AppRow
	Status         string
	LastDeployed   *string
	ActiveWorkers  int
	ActiveSessions int
	TotalViews     int
	RecentViews    int
	ActiveBundle   *db.BundleRow
}

type settingsTabData struct {
	App           *db.AppRow
	Status        string
	Tags          []db.TagRow
	AvailableTags []db.TagRow
	ActiveBundle  *db.BundleRow
}

type runtimeTabData struct {
	App            *db.AppRow
	Workers        []runtimeWorkerView
	ActiveSessions int
	UniqueVisitors int
	TotalViews     int
	RecentViews    int
}

type runtimeWorkerView struct {
	ID               string
	Status           string
	CPUPercent       float64
	MemoryUsageBytes uint64
	MemoryLimitBytes uint64
	Sessions         []runtimeSessionView
}

type runtimeSessionView struct {
	UserDisplayName string
	StartedAt       *string
}

type bundlesTabData struct {
	App          *db.AppRow
	Bundles      []db.BundleRow
	ActiveBundle *db.BundleRow
}

type collaboratorsTabData struct {
	App    *db.AppRow
	Grants []db.AccessGrantWithName
}

type logsTabData struct {
	App     *db.AppRow
	Workers []logWorkerEntry
}

type logWorkerEntry struct {
	ID           string
	Status       string
	SessionCount int
	StartedAt    *string
}

type logWorkerViewData struct {
	AppID          string
	App            *db.AppRow
	WorkerID       string
	Active         bool
	HistoricalLogs string
}

// --- Shared helpers ---

// uiResolveApp looks up an app by UUID first, then by name.
func uiResolveApp(database *db.DB, nameOrID string) (*db.AppRow, error) {
	app, err := database.GetApp(nameOrID)
	if err != nil {
		return nil, err
	}
	if app != nil {
		return app, nil
	}
	return database.GetAppByName(nameOrID)
}

// uiEvaluateRelation computes the caller's relationship to an app.
func uiEvaluateRelation(srv *server.Server, caller *auth.CallerIdentity, app *db.AppRow) authz.AppRelation {
	rows, err := srv.DB.ListAppAccess(app.ID)
	if err != nil {
		return authz.RelationNone
	}
	grants := make([]authz.AccessGrant, len(rows))
	for i, row := range rows {
		role, _ := authz.ParseContentRole(row.Role)
		grants[i] = authz.AccessGrant{
			AppID:     row.AppID,
			Principal: row.Principal,
			Kind:      authz.AccessKind(row.Kind),
			Role:      role,
			GrantedBy: row.GrantedBy,
			GrantedAt: row.GrantedAt,
		}
	}
	return authz.EvaluateAccess(caller, app.Owner, grants, app.AccessType)
}

// resolveAppForFragment is the shared auth + app resolution for all
// sidebar fragment handlers. Returns nil app on failure (404 already written).
func (ui *UI) resolveAppForFragment(
	srv *server.Server,
	w http.ResponseWriter,
	r *http.Request,
	minRelation authz.AppRelation,
) (*db.AppRow, authz.AppRelation) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		w.WriteHeader(http.StatusNotFound)
		return nil, authz.RelationNone
	}

	name := chi.URLParam(r, "name")
	app, err := uiResolveApp(srv.DB, name)
	if err != nil || app == nil {
		w.WriteHeader(http.StatusNotFound)
		return nil, authz.RelationNone
	}

	caller := auth.CallerFromContext(r.Context())
	relation := uiEvaluateRelation(srv, caller, app)
	if relation < minRelation {
		w.WriteHeader(http.StatusNotFound)
		return nil, authz.RelationNone
	}

	return app, relation
}

// computeAppStatus derives display status from worker state.
func computeAppStatus(srv *server.Server, app *db.AppRow) string {
	if !app.Enabled {
		return "disabled"
	}
	workerIDs := srv.Workers.ForApp(app.ID)
	if len(workerIDs) == 0 {
		return "stopped"
	}
	allDraining := true
	for _, wid := range workerIDs {
		w, ok := srv.Workers.Get(wid)
		if ok && !w.Draining {
			allDraining = false
			break
		}
	}
	if allDraining {
		return "stopping"
	}
	return "running"
}

// getActiveBundle fetches the active bundle for an app, or nil.
func getActiveBundle(srv *server.Server, app *db.AppRow) *db.BundleRow {
	if app.ActiveBundle == nil {
		return nil
	}
	b, err := srv.DB.GetBundle(*app.ActiveBundle)
	if err != nil {
		return nil
	}
	return b
}

// --- Sidebar shell handler ---

func (ui *UI) sidebarHandler(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, relation := ui.resolveAppForFragment(srv, w, r, authz.RelationContentCollaborator)
		if app == nil {
			return
		}

		// Pre-render overview tab content.
		overviewData := ui.buildOverviewData(srv, app)
		var buf bytes.Buffer
		if err := ui.fragments["tab_overview.html"].Execute(&buf, overviewData); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		data := sidebarData{
			App:          app,
			CanManageACL: relation.CanManageACL(),
			OverviewHTML: template.HTML(buf.String()),
		}

		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["sidebar.html"].Execute(w, data); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

// --- Tab handlers ---

func (ui *UI) overviewTab(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, _ := ui.resolveAppForFragment(srv, w, r, authz.RelationContentCollaborator)
		if app == nil {
			return
		}

		data := ui.buildOverviewData(srv, app)
		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["tab_overview.html"].Execute(w, data); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

func (ui *UI) buildOverviewData(srv *server.Server, app *db.AppRow) overviewTabData {
	workerIDs := srv.Workers.ForApp(app.ID)
	activeSessionCount := srv.Sessions.CountForWorkers(workerIDs)

	since := time.Now().AddDate(0, 0, -7).UTC()
	totalViews, _ := srv.DB.CountSessions(app.ID)
	recentViews, _ := srv.DB.CountRecentSessions(app.ID, since)

	bundle := getActiveBundle(srv, app)
	var lastDeployed *string
	if bundle != nil {
		lastDeployed = bundle.DeployedAt
	}

	return overviewTabData{
		App:            app,
		Status:         computeAppStatus(srv, app),
		LastDeployed:   lastDeployed,
		ActiveWorkers:  len(workerIDs),
		ActiveSessions: activeSessionCount,
		TotalViews:     totalViews,
		RecentViews:    recentViews,
		ActiveBundle:   bundle,
	}
}

func (ui *UI) settingsTab(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, _ := ui.resolveAppForFragment(srv, w, r, authz.RelationContentCollaborator)
		if app == nil {
			return
		}

		tags, _ := srv.DB.ListAppTags(app.ID)
		allTags, _ := srv.DB.ListTags()

		// Filter available tags to those not already applied.
		appliedIDs := make(map[string]bool, len(tags))
		for _, t := range tags {
			appliedIDs[t.ID] = true
		}
		var available []db.TagRow
		for _, t := range allTags {
			if !appliedIDs[t.ID] {
				available = append(available, t)
			}
		}

		data := settingsTabData{
			App:           app,
			Status:        computeAppStatus(srv, app),
			Tags:          tags,
			AvailableTags: available,
			ActiveBundle:  getActiveBundle(srv, app),
		}

		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["tab_settings.html"].Execute(w, data); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

func (ui *UI) runtimeTab(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, _ := ui.resolveAppForFragment(srv, w, r, authz.RelationContentCollaborator)
		if app == nil {
			return
		}

		workerIDs := srv.Workers.ForApp(app.ID)
		var workers []runtimeWorkerView
		totalSessions := 0

		for _, wid := range workerIDs {
			aw, ok := srv.Workers.Get(wid)
			if !ok {
				continue
			}

			status := "active"
			if aw.Draining {
				status = "draining"
			}

			wv := runtimeWorkerView{
				ID:     wid,
				Status: status,
			}

			// Container stats (best-effort).
			stats, err := srv.Backend.ContainerStats(context.Background(), wid)
			if err == nil && stats != nil {
				wv.CPUPercent = stats.CPUPercent
				wv.MemoryUsageBytes = stats.MemoryUsageBytes
				wv.MemoryLimitBytes = stats.MemoryLimitBytes
			}

			// Sessions.
			entries := srv.Sessions.EntriesForWorker(wid)
			for _, entry := range entries {
				displayName := entry.UserSub
				if displayName != "" {
					if u, err := srv.DB.GetUser(entry.UserSub); err == nil && u != nil && u.Name != "" {
						displayName = u.Name
					}
				} else {
					displayName = "anonymous"
				}
				startedAt := entry.LastAccess.UTC().Format(time.RFC3339)
				wv.Sessions = append(wv.Sessions, runtimeSessionView{
					UserDisplayName: displayName,
					StartedAt:       &startedAt,
				})
			}
			totalSessions += len(entries)

			workers = append(workers, wv)
		}

		since := time.Now().AddDate(0, 0, -7).UTC()
		totalViews, _ := srv.DB.CountSessions(app.ID)
		recentViews, _ := srv.DB.CountRecentSessions(app.ID, since)
		uniqueVisitors, _ := srv.DB.CountUniqueVisitors(app.ID)

		data := runtimeTabData{
			App:            app,
			Workers:        workers,
			ActiveSessions: totalSessions,
			UniqueVisitors: uniqueVisitors,
			TotalViews:     totalViews,
			RecentViews:    recentViews,
		}

		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["tab_runtime.html"].Execute(w, data); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

func (ui *UI) bundlesTab(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, _ := ui.resolveAppForFragment(srv, w, r, authz.RelationContentCollaborator)
		if app == nil {
			return
		}

		bundles, _ := srv.DB.ListBundlesByApp(app.ID)
		data := bundlesTabData{
			App:          app,
			Bundles:      bundles,
			ActiveBundle: getActiveBundle(srv, app),
		}

		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["tab_bundles.html"].Execute(w, data); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

func (ui *UI) collaboratorsTab(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, _ := ui.resolveAppForFragment(srv, w, r, authz.RelationOwner)
		if app == nil {
			return
		}

		grants, _ := srv.DB.ListAppAccessWithNames(app.ID)
		data := collaboratorsTabData{
			App:    app,
			Grants: grants,
		}

		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["tab_collaborators.html"].Execute(w, data); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

func (ui *UI) logsTab(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, _ := ui.resolveAppForFragment(srv, w, r, authz.RelationContentCollaborator)
		if app == nil {
			return
		}

		var workers []logWorkerEntry

		// Live workers from WorkerMap.
		for _, wid := range srv.Workers.ForApp(app.ID) {
			aw, ok := srv.Workers.Get(wid)
			if !ok {
				continue
			}
			status := "active"
			if aw.Draining {
				status = "draining"
			}
			startedAt := aw.StartedAt.UTC().Format(time.RFC3339)
			sessionCount := srv.Sessions.CountForWorker(wid)
			workers = append(workers, logWorkerEntry{
				ID:           wid,
				Status:       status,
				SessionCount: sessionCount,
				StartedAt:    &startedAt,
			})
		}

		// Dead workers from logstore.
		if srv.LogStore != nil {
			for _, info := range srv.LogStore.WorkerIDsByApp(app.ID) {
				if !info.Ended {
					continue // already listed as live worker
				}
				endedAt := info.EndedAt.UTC().Format(time.RFC3339)
				startedAt := info.StartedAt.UTC().Format(time.RFC3339)
				_ = endedAt // not displayed but could be used later
				workers = append(workers, logWorkerEntry{
					ID:           info.ID,
					Status:       "ended",
					SessionCount: 0,
					StartedAt:    &startedAt,
				})
			}
		}

		data := logsTabData{
			App:     app,
			Workers: workers,
		}

		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["tab_logs.html"].Execute(w, data); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

func (ui *UI) logsWorkerTab(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, _ := ui.resolveAppForFragment(srv, w, r, authz.RelationContentCollaborator)
		if app == nil {
			return
		}

		workerID := chi.URLParam(r, "wid")
		if workerID == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// Determine if the worker is active.
		_, active := srv.Workers.Get(workerID)

		// Get historical logs from logstore.
		var historicalLogs string
		if srv.LogStore != nil {
			snapshot, _, ok := srv.LogStore.Subscribe(workerID)
			if ok && len(snapshot) > 0 {
				historicalLogs = strings.Join(snapshot, "\n")
			}
		}

		data := logWorkerViewData{
			AppID:          app.ID,
			App:            app,
			WorkerID:       workerID,
			Active:         active,
			HistoricalLogs: historicalLogs,
		}

		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["tab_logs_worker.html"].Execute(w, data); err != nil {
			fmt.Fprintf(w, `<p class="error-message">Failed to load logs.</p>`)
		}
	}
}
