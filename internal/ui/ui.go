package ui

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/preflight"
	"github.com/cynkra/blockyard/internal/server"
)

//go:embed templates/*.html static/*
var content embed.FS

// UI holds parsed templates and the static file handler.
// Page templates are parsed with base.html (full-page renders).
// Fragment templates are parsed standalone (htmx partial responses).
type UI struct {
	pages     map[string]*template.Template
	fragments map[string]*template.Template
	static    http.Handler
}

var funcMap = template.FuncMap{
	"deref": func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	},
	"derefInt": func(p *int) string {
		if p == nil {
			return ""
		}
		return strconv.Itoa(*p)
	},
	"derefFloat": func(p *float64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatFloat(*p, 'f', -1, 64)
	},
	"timeAgo": func(v any) string {
		var s string
		switch val := v.(type) {
		case *string:
			if val == nil {
				return ""
			}
			s = *val
		case string:
			if val == "" {
				return ""
			}
			s = val
		default:
			return ""
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return s
		}
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			m := int(d.Minutes())
			if m == 1 {
				return "1 minute ago"
			}
			return fmt.Sprintf("%d minutes ago", m)
		case d < 24*time.Hour:
			h := int(d.Hours())
			if h == 1 {
				return "1 hour ago"
			}
			return fmt.Sprintf("%d hours ago", h)
		default:
			days := int(d.Hours() / 24)
			if days == 1 {
				return "1 day ago"
			}
			return fmt.Sprintf("%d days ago", days)
		}
	},
	"timeUntil": func(v any) string {
		var s string
		switch val := v.(type) {
		case *string:
			if val == nil {
				return ""
			}
			s = *val
		case string:
			if val == "" {
				return ""
			}
			s = val
		default:
			return ""
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return s
		}
		d := time.Until(t)
		if d < 0 {
			return "Expired"
		}
		switch {
		case d < time.Hour:
			m := int(d.Minutes())
			if m <= 1 {
				return "in <1 minute"
			}
			return fmt.Sprintf("in %d minutes", m)
		case d < 24*time.Hour:
			h := int(d.Hours())
			if h == 1 {
				return "in 1 hour"
			}
			return fmt.Sprintf("in %d hours", h)
		default:
			days := int(d.Hours() / 24)
			if days == 1 {
				return "in 1 day"
			}
			return fmt.Sprintf("in %d days", days)
		}
	},
	"truncate": func(s string) string {
		if len(s) <= 8 {
			return s
		}
		return s[:8] + "..."
	},
	"humanBytes": func(b uint64) string {
		const (
			KB = 1024
			MB = KB * 1024
			GB = MB * 1024
		)
		switch {
		case b >= GB:
			return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
		case b >= MB:
			return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
		case b >= KB:
			return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
		default:
			return fmt.Sprintf("%d B", b)
		}
	},
	"add": func(a, b int) int {
		return a + b
	},
	"subtract": func(a, b int) int {
		return a - b
	},
	"statusDotClass": func(status string) string {
		switch status {
		case "running", "active":
			return "status-success"
		case "idle":
			return "status-idle"
		case "ready", "configured":
			return "status-neutral"
		case "building":
			return "status-info"
		case "stopping", "draining":
			return "status-warning"
		case "error", "failed":
			return "status-error"
		case "disabled", "stopped":
			return "status-outline"
		default:
			return "status-neutral"
		}
	},
	"contains": func(slice []string, val string) bool {
		for _, s := range slice {
			if s == val {
				return true
			}
		}
		return false
	},
}

// New parses all embedded templates and prepares the static file server.
func New() *UI {
	pages := make(map[string]*template.Template)
	for _, name := range []string{"landing.html", "apps.html", "deployments.html", "api_keys.html", "profile.html", "admin.html"} {
		t := template.Must(
			template.New("").Funcs(funcMap).ParseFS(
				content, "templates/base.html", "templates/icons.html", "templates/"+name,
			),
		)
		pages[name] = t
	}
	// Re-parse profile.html with the shared token_list fragment.
	pages["profile.html"] = template.Must(
		template.New("").Funcs(funcMap).ParseFS(
			content, "templates/base.html", "templates/icons.html", "templates/profile.html", "templates/token_list.html",
		),
	)
	// Re-parse admin.html with the shared system_checks and admin_users fragments.
	pages["admin.html"] = template.Must(
		template.New("").Funcs(funcMap).ParseFS(
			content,
			"templates/base.html", "templates/icons.html",
			"templates/admin.html",
			"templates/system_checks.html",
			"templates/admin_users.html",
		),
	)

	fragments := make(map[string]*template.Template)
	fragmentNames := []string{
		"token_list.html",
		"pat_created.html",
		"system_checks.html",
		"system_banner.html",
		"admin_users.html",
		"sidebar.html",
		"tab_overview.html",
		"tab_settings.html",
		"tab_runtime.html",
		"tab_runtime_worker.html",
		"tab_bundles.html",
		"tab_collaborators.html",
		"tab_logs.html",
		"tab_logs_worker.html",
		"error_fragment.html",
		"new_app_modal.html",
	}
	for _, name := range fragmentNames {
		t := template.Must(
			template.New(name).Funcs(funcMap).ParseFS(
				content, "templates/"+name,
			),
		)
		fragments[name] = t
	}

	static := http.FileServer(http.FS(content))
	return &UI{pages: pages, fragments: fragments, static: static}
}

// RegisterRoutes mounts the UI routes on the router.
func (ui *UI) RegisterRoutes(r chi.Router, srv *server.Server) {
	r.Get("/", ui.appsPage(srv))
	r.Get("/deployments", ui.deploymentsPage(srv))
	r.Get("/api-keys", ui.apiKeysPage(srv))
	r.Get("/admin", ui.adminPage(srv))
	r.Get("/ui/admin/users", ui.adminUsersFragment(srv))
	r.Get("/profile", ui.profilePage(srv))
	r.Post("/ui/tokens", ui.createToken(srv))
	r.Post("/ui/system/run", ui.systemRunFragment(srv))
	r.Get("/ui/system/banner", ui.systemBannerFragment(srv))

	// New-app upload routes (before {name} wildcard).
	r.Get("/ui/apps/new", ui.newAppForm(srv))
	r.Post("/ui/apps/new", ui.createApp(srv))

	// Sidebar fragment routes (phase 2-11).
	r.Get("/ui/apps/{name}/sidebar", ui.sidebarHandler(srv))
	r.Get("/ui/apps/{name}/tab/overview", ui.overviewTab(srv))
	r.Get("/ui/apps/{name}/tab/settings", ui.settingsTab(srv))
	r.Get("/ui/apps/{name}/tab/runtime", ui.runtimeTab(srv))
	r.Get("/ui/apps/{name}/tab/bundles", ui.bundlesTab(srv))
	r.Get("/ui/apps/{name}/tab/collaborators", ui.collaboratorsTab(srv))
	r.Get("/ui/apps/{name}/tab/logs", ui.logsTab(srv))
	r.Get("/ui/apps/{name}/tab/logs/worker/{wid}", ui.logsWorkerTab(srv))
	r.Get("/ui/apps/{name}/tab/runtime/worker/{wid}", ui.workerDetailTab(srv))
	r.Get("/ui/deployments/{bundleID}/logs", ui.deploymentLogFragment(srv))
	r.Get("/ui/users/search", ui.searchUsersFragment(srv))
	r.Post("/ui/apps/{name}/tags", ui.createAndAssignTag(srv))

	r.Handle("/static/*", ui.static)
}

// --- Shared layout data ---

type layoutData struct {
	ActivePage     string // "apps", "deployments", "api-keys", "admin", "profile"; empty for landing
	OpenbaoEnabled bool
	IsAdmin        bool
	Version        string
}

// --- Page data types ---

type landingData struct {
	layoutData
	PublicApps []catalogEntry
}

type appsData struct {
	layoutData
	UserRole   string
	Search     string
	ActiveTags []string
	TagMode    string // "and" or "or"
	AllTags    []db.TagWithCount
	Sort       string
	SortDir    string
	Apps       []catalogEntry
}

type deploymentsData struct {
	layoutData
	Search      string
	Sort        string
	SortDir     string
	Deployments []deploymentEntry
	Pagination  paginationData
}

type apiKeysData struct {
	layoutData
	Services []serviceEntry
}

type profileData struct {
	layoutData
	User   profileUser
	Tokens []tokenEntry
}

type systemData struct {
	layoutData
	Report *preflight.Report
}

type adminData struct {
	layoutData
	Report *preflight.Report
	Users  adminUsersData
}

type adminUsersData struct {
	CallerSub    string
	Users        []adminUserEntry
	Search       string
	Role         string
	ActiveFilter string
	Sort         string
	SortDir      string
	Pagination   paginationData
}

type adminUserEntry struct {
	Sub       string
	Name      string
	Email     string
	Role      string
	Active    bool
	LastLogin string
	IsSelf    bool
}

type catalogEntry struct {
	ID          string
	Name        string
	Title       *string
	Description *string
	Status      string
	Tags        []string
	CanManage   bool
}

type deploymentEntry struct {
	AppName        string
	BundleID       string
	DeployedByName string
	DeployedAt     *string
	Status         string
}

type serviceEntry struct {
	ID     string
	Label  string
	Status string // "configured" or "not_set"
}

type profileUser struct {
	DisplayName string
	Email       string
	Role        string
}

type tokenEntry struct {
	ID         string
	Name       string
	CreatedAt  *string
	ExpiresAt  *string
	LastUsedAt *string
}

type paginationData struct {
	Page       int
	TotalPages int
	Search     string
}

const deploymentsPerPage = 20

// --- Auth helpers ---

// requireAuth checks for an authenticated user. If not authenticated,
// redirects GET requests to /login?return_url=<path> and returns 401
// for non-GET requests. Returns nil if not authenticated.
func requireAuth(w http.ResponseWriter, r *http.Request) *auth.AuthenticatedUser {
	user := auth.UserFromContext(r.Context())
	if user != nil {
		return user
	}
	if r.Method == http.MethodGet {
		http.Redirect(w, r, "/login?return_url="+url.QueryEscape(r.URL.Path), http.StatusFound)
	} else {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
	return nil
}

func openbaoEnabled(srv *server.Server) bool {
	return srv.Config.Openbao != nil && len(srv.Config.Openbao.Services) > 0
}

func baseLayout(srv *server.Server, activePage string, caller *auth.CallerIdentity) layoutData {
	isAdmin := caller != nil && caller.Role.CanManageRoles()
	return layoutData{
		ActivePage:     activePage,
		OpenbaoEnabled: openbaoEnabled(srv),
		IsAdmin:        isAdmin,
		Version:        srv.Version,
	}
}

// --- Page handlers ---

func (ui *UI) appsPage(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		if user == nil {
			ui.renderLanding(w, r, srv)
			return
		}

		caller := auth.CallerFromContext(r.Context())

		search := r.URL.Query().Get("search")
		activeTags := r.URL.Query()["tag"]
		tagMode := r.URL.Query().Get("tag_mode")
		if tagMode == "" {
			tagMode = "and"
		}
		sort := r.URL.Query().Get("sort")
		sortDir := r.URL.Query().Get("dir")

		params := db.CatalogParams{
			Search:  search,
			Tags:    activeTags,
			TagMode: tagMode,
			Sort:    sort,
			SortDir: sortDir,
			Page:    1,
			PerPage: 100,
		}
		if caller != nil {
			params.CallerSub = caller.Sub
			params.CallerRole = caller.Role.String()
		}

		apps, _, err := srv.DB.ListCatalogWithRelation(params)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		allTags, _ := srv.DB.ListTagsWithCounts()
		entries := buildCatalogEntries(apps, srv)

		role := "none"
		if caller != nil {
			role = caller.Role.String()
		}

		data := appsData{
			layoutData: baseLayout(srv, "apps", caller),
			UserRole:   role,
			Search:     search,
			ActiveTags: activeTags,
			TagMode:    tagMode,
			AllTags:    allTags,
			Sort:       sort,
			SortDir:    sortDir,
			Apps:       entries,
		}

		if err := ui.pages["apps.html"].ExecuteTemplate(w, "base", data); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

func (ui *UI) renderLanding(w http.ResponseWriter, r *http.Request, srv *server.Server) {
	apps, _, err := srv.DB.ListCatalog(db.CatalogParams{
		Page:    1,
		PerPage: 100,
	})
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	entries := buildLandingEntries(apps, srv)
	if err := ui.pages["landing.html"].ExecuteTemplate(w, "base", landingData{
		layoutData: baseLayout(srv, "", nil),
		PublicApps: entries,
	}); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
	}
}

func (ui *UI) deploymentsPage(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := requireAuth(w, r)
		if user == nil {
			return
		}

		caller := auth.CallerFromContext(r.Context())

		search := r.URL.Query().Get("search")
		sort := r.URL.Query().Get("sort")
		sortDir := r.URL.Query().Get("dir")
		page := 1
		if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
			page = p
		}

		opts := db.DeploymentListOpts{
			Search:  search,
			Sort:    sort,
			SortDir: sortDir,
			Page:    page,
			PerPage: deploymentsPerPage,
		}
		if caller != nil {
			opts.CallerSub = caller.Sub
			opts.CallerRole = caller.Role.String()
		}

		rows, total, err := srv.DB.ListDeployments(opts)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		entries := make([]deploymentEntry, len(rows))
		for i, r := range rows {
			name := ""
			if r.DeployedByName != nil {
				name = *r.DeployedByName
			} else if r.DeployedBy != nil {
				name = *r.DeployedBy
			}
			entries[i] = deploymentEntry{
				AppName:        r.AppName,
				BundleID:       r.BundleID,
				DeployedByName: name,
				DeployedAt:     r.DeployedAt,
				Status:         r.Status,
			}
		}

		totalPages := int(math.Ceil(float64(total) / float64(deploymentsPerPage)))
		if totalPages < 1 {
			totalPages = 1
		}

		data := deploymentsData{
			layoutData:  baseLayout(srv, "deployments", caller),
			Search:      search,
			Sort:        sort,
			SortDir:     sortDir,
			Deployments: entries,
			Pagination: paginationData{
				Page:       page,
				TotalPages: totalPages,
				Search:     search,
			},
		}

		if err := ui.pages["deployments.html"].ExecuteTemplate(w, "base", data); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

func (ui *UI) apiKeysPage(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !openbaoEnabled(srv) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}

		user := requireAuth(w, r)
		if user == nil {
			return
		}

		caller := auth.CallerFromContext(r.Context())
		services := buildServiceEntries(srv, user.Sub)

		data := apiKeysData{
			layoutData: baseLayout(srv, "api-keys", caller),
			Services:   services,
		}

		if err := ui.pages["api_keys.html"].ExecuteTemplate(w, "base", data); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

func (ui *UI) profilePage(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := requireAuth(w, r)
		if user == nil {
			return
		}

		caller := auth.CallerFromContext(r.Context())

		displayName := user.Sub
		email := ""
		role := "viewer"
		if caller != nil {
			displayName = caller.DisplayName()
			role = caller.Role.String()
		}

		// Look up email from DB
		dbUser, err := srv.DB.GetUser(user.Sub)
		if err == nil && dbUser != nil {
			email = dbUser.Email
			if dbUser.Name != "" {
				displayName = dbUser.Name
			}
		}

		// List PATs
		pats, _ := srv.DB.ListPATsByUser(user.Sub)
		tokens := make([]tokenEntry, len(pats))
		for i, p := range pats {
			createdAt := p.CreatedAt
			tokens[i] = tokenEntry{
				ID:         p.ID,
				Name:       p.Name,
				CreatedAt:  &createdAt,
				ExpiresAt:  p.ExpiresAt,
				LastUsedAt: p.LastUsedAt,
			}
		}

		data := profileData{
			layoutData: baseLayout(srv, "profile", caller),
			User: profileUser{
				DisplayName: displayName,
				Email:       email,
				Role:        role,
			},
			Tokens: tokens,
		}

		if err := ui.pages["profile.html"].ExecuteTemplate(w, "base", data); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

func (ui *UI) adminPage(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := requireAuth(w, r)
		if user == nil {
			return
		}

		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageRoles() {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}

		usersData, err := buildAdminUsers(srv, caller, r.URL.Query())
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		var report *preflight.Report
		if srv.Checker != nil {
			report = srv.Checker.Latest()
		}

		data := adminData{
			layoutData: baseLayout(srv, "admin", caller),
			Report:     report,
			Users:      usersData,
		}

		if err := ui.pages["admin.html"].ExecuteTemplate(w, "base", data); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

// adminUsersFragment serves the users table fragment for HTMX refreshes
// driven by filter/pagination/sort controls.
func (ui *UI) adminUsersFragment(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageRoles() {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		usersData, err := buildAdminUsers(srv, caller, r.URL.Query())
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Mirror the filter/pagination state into the browser URL so
		// reloads land on the same view.
		if pushURL := buildAdminPushURL(r.URL.Query()); pushURL != "" {
			w.Header().Set("HX-Push-Url", pushURL)
		}

		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["admin_users.html"].ExecuteTemplate(w, "adminUsersTable", usersData); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

// buildAdminPushURL returns the /admin URL with only the user-visible
// filter/pagination params, omitting empty values.
func buildAdminPushURL(q url.Values) string {
	keep := url.Values{}
	for _, k := range []string{"search", "role", "active", "sort", "dir", "page"} {
		if v := q.Get(k); v != "" {
			keep.Set(k, v)
		}
	}
	if len(keep) == 0 {
		return "/admin"
	}
	return "/admin?" + keep.Encode()
}

const adminUsersPerPage = 20

func buildAdminUsers(srv *server.Server, caller *auth.CallerIdentity, q url.Values) (adminUsersData, error) {
	search := q.Get("search")
	role := q.Get("role")
	activeFilter := q.Get("active")
	sort := q.Get("sort")
	sortDir := q.Get("dir")
	page := 1
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		page = p
	}

	opts := db.ListUsersOpts{
		Search:       search,
		Role:         role,
		ActiveFilter: activeFilter,
		Sort:         sort,
		SortDir:      sortDir,
		Page:         page,
		PerPage:      adminUsersPerPage,
	}

	rows, total, err := srv.DB.ListUsers(opts)
	if err != nil {
		return adminUsersData{}, err
	}

	entries := make([]adminUserEntry, len(rows))
	for i, u := range rows {
		entries[i] = adminUserEntry{
			Sub:       u.Sub,
			Name:      u.Name,
			Email:     u.Email,
			Role:      u.Role,
			Active:    u.Active,
			LastLogin: u.LastLogin,
			IsSelf:    caller != nil && u.Sub == caller.Sub,
		}
	}

	totalPages := int(math.Ceil(float64(total) / float64(adminUsersPerPage)))
	if totalPages < 1 {
		totalPages = 1
	}

	return adminUsersData{
		CallerSub:    caller.Sub,
		Users:        entries,
		Search:       search,
		Role:         role,
		ActiveFilter: activeFilter,
		Sort:         sort,
		SortDir:      sortDir,
		Pagination: paginationData{
			Page:       page,
			TotalPages: totalPages,
			Search:     search,
		},
	}, nil
}

func (ui *UI) systemRunFragment(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || caller.Role < auth.RoleAdmin {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		report := srv.Checker.RunDynamic(r.Context())

		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["system_checks.html"].ExecuteTemplate(w, "checkResults", systemData{
			Report: report,
		}); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

func (ui *UI) systemBannerFragment(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		isAdmin := caller != nil && caller.Role >= auth.RoleAdmin

		var hasWarnings bool
		var errors, warnings int
		if isAdmin && srv.Checker != nil {
			if report := srv.Checker.Latest(); report != nil {
				hasWarnings = report.HasWarnings()
				errors = report.Summary.Errors
				warnings = report.Summary.Warnings
			}
		}

		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["system_banner.html"].Execute(w, struct {
			IsAdmin     bool
			HasWarnings bool
			Errors      int
			Warnings    int
		}{isAdmin, hasWarnings, errors, warnings}); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
	}
}

func (ui *UI) createToken(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if caller.Source != auth.AuthSourceSession {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		name := strings.TrimSpace(r.FormValue("name")) //nolint:gosec // G120: auth-gated endpoint, bounded board name
		if name == "" {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `<p class="pat-error">Token name is required.</p>`)
			return
		}

		plaintext, hash, err := auth.GeneratePAT()
		if err != nil {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `<p class="pat-error">Failed to generate token.</p>`)
			return
		}

		var expiresAt *string
		if v := r.FormValue("expires_in"); v != "" { //nolint:gosec // G120: auth-gated endpoint, bounded select value
			dur, ok := parseDuration(v)
			if !ok {
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, `<p class="pat-error">Invalid expiry value.</p>`)
				return
			}
			exp := time.Now().Add(dur).UTC().Format(time.RFC3339)
			expiresAt = &exp
		}

		id := uuid.New().String()
		_, err = srv.DB.CreatePAT(id, hash, caller.Sub, name, expiresAt)
		if err != nil {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `<p class="pat-error">Failed to create token.</p>`)
			return
		}

		w.Header().Set("Content-Type", "text/html")
		if err := ui.fragments["pat_created.html"].Execute(w, struct {
			Token     string
			ExpiresAt *string
		}{Token: plaintext, ExpiresAt: expiresAt}); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// OOB swap: refresh the token list table.
		pats, _ := srv.DB.ListPATsByUser(caller.Sub)
		tokens := make([]tokenEntry, len(pats))
		for i, p := range pats {
			createdAt := p.CreatedAt
			tokens[i] = tokenEntry{
				ID:         p.ID,
				Name:       p.Name,
				CreatedAt:  &createdAt,
				LastUsedAt: p.LastUsedAt,
			}
		}
		fmt.Fprint(w, `<div id="token-list" hx-swap-oob="true">`)
		_ = ui.fragments["token_list.html"].ExecuteTemplate(w, "tokenList", tokens)
		fmt.Fprint(w, `</div>`)
	}
}

// parseDuration parses duration strings like "90d", "24h", "30m".
var durationRe = regexp.MustCompile(`^(\d+)([dhm])$`)

func parseDuration(s string) (time.Duration, bool) {
	m := durationRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	switch m[2] {
	case "d":
		return time.Duration(n) * 24 * time.Hour, true
	case "h":
		return time.Duration(n) * time.Hour, true
	case "m":
		return time.Duration(n) * time.Minute, true
	}
	return 0, false
}

// --- Builders ---

func buildCatalogEntries(apps []db.CatalogRow, srv *server.Server) []catalogEntry {
	entries := make([]catalogEntry, 0, len(apps))
	for _, app := range apps {
		var tagNames []string
		if app.Tags != "" {
			tagNames = strings.Split(app.Tags, ",")
		}

		status := "disabled"
		if app.Enabled {
			status = "idle"
			if srv.Workers.CountForApp(app.ID) > 0 {
				status = "running"
			}
		}

		canManage := app.Relation == "admin" || app.Relation == "owner" || app.Relation == "collaborator"

		entries = append(entries, catalogEntry{
			ID:          app.ID,
			Name:        app.Name,
			Title:       app.Title,
			Description: app.Description,
			Status:      status,
			Tags:        tagNames,
			CanManage:   canManage,
		})
	}
	return entries
}

func buildLandingEntries(apps []db.AppRow, srv *server.Server) []catalogEntry {
	entries := make([]catalogEntry, 0, len(apps))
	for _, app := range apps {
		tags, _ := srv.DB.ListAppTags(app.ID)
		tagNames := make([]string, len(tags))
		for i, t := range tags {
			tagNames[i] = t.Name
		}

		status := "disabled"
		if app.Enabled {
			status = "idle"
			if srv.Workers.CountForApp(app.ID) > 0 {
				status = "running"
			}
		}

		entries = append(entries, catalogEntry{
			ID:          app.ID,
			Name:        app.Name,
			Title:       app.Title,
			Description: app.Description,
			Status:      status,
			Tags:        tagNames,
		})
	}
	return entries
}

func buildServiceEntries(srv *server.Server, sub string) []serviceEntry {
	entries := make([]serviceEntry, 0, len(srv.Config.Openbao.Services))
	for _, svc := range srv.Config.Openbao.Services {
		status := "not_set"
		if srv.VaultClient != nil {
			exists, err := srv.VaultClient.SecretExists(
				context.Background(),
				"secret/data/users/"+sub+"/apikeys/"+svc.ID,
			)
			if err == nil && exists {
				status = "configured"
			}
		}
		entries = append(entries, serviceEntry{
			ID:     svc.ID,
			Label:  svc.Label,
			Status: status,
		})
	}
	return entries
}
