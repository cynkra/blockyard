package ui

import (
	"context"
	"embed"
	"html/template"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

//go:embed templates/*.html static/*
var content embed.FS

// UI holds parsed templates and the static file handler.
// Each page template is parsed separately with the base template to
// avoid Go's template define-block collision between pages.
type UI struct {
	pages  map[string]*template.Template
	static http.Handler
}

var funcMap = template.FuncMap{
	"deref": func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	},
}

// New parses all embedded templates and prepares the static file server.
func New() *UI {
	pages := make(map[string]*template.Template)
	for _, name := range []string{"landing.html", "dashboard.html"} {
		t := template.Must(
			template.New("").Funcs(funcMap).ParseFS(
				content, "templates/base.html", "templates/"+name,
			),
		)
		pages[name] = t
	}
	static := http.FileServer(http.FS(content))
	return &UI{pages: pages, static: static}
}

// RegisterRoutes mounts the UI routes on the router.
func (ui *UI) RegisterRoutes(r chi.Router, srv *server.Server) {
	r.Get("/", ui.root(srv))
	r.Handle("/static/*", ui.static)
}

func (ui *UI) root(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check session
		user := auth.UserFromContext(r.Context())
		if user == nil {
			ui.renderLanding(w, r, srv)
			return
		}

		ui.renderDashboard(w, r, srv, user)
	}
}

// --- Data types for templates ---

type landingData struct {
	PublicApps []catalogEntry
}

type dashboardData struct {
	UserSub           string
	UserRole          string
	Search            string
	ActiveTag         string
	AllTags           []db.TagRow
	Apps              []catalogEntry
	Services          []serviceEntry
	CredentialError   string
	CredentialSuccess bool
}

type catalogEntry struct {
	ID          string
	Name        string
	Title       *string
	Description *string
	Status      string
	Tags        []string
}

type serviceEntry struct {
	ID     string
	Label  string
	Status string // "configured" or "not_set"
}

// --- Handlers ---

func (ui *UI) renderLanding(w http.ResponseWriter, r *http.Request, srv *server.Server) {
	apps, _, err := srv.DB.ListCatalog(db.CatalogParams{
		Page:    1,
		PerPage: 100,
	})
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	entries := buildCatalogEntries(apps, srv)
	ui.pages["landing.html"].ExecuteTemplate(w, "base", landingData{
		PublicApps: entries,
	})
}

func (ui *UI) renderDashboard(w http.ResponseWriter, r *http.Request, srv *server.Server, user *auth.AuthenticatedUser) {
	caller := auth.CallerFromContext(r.Context())

	search := r.URL.Query().Get("search")
	activeTag := r.URL.Query().Get("tag")

	params := db.CatalogParams{
		Search:  search,
		Tag:     activeTag,
		Page:    1,
		PerPage: 100,
	}
	if caller != nil {
		params.CallerSub = caller.Sub
		params.CallerRole = caller.Role.String()
	}

	apps, _, err := srv.DB.ListCatalog(params)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	allTags, _ := srv.DB.ListTags()
	entries := buildCatalogEntries(apps, srv)

	role := "none"
	if caller != nil {
		role = caller.Role.String()
	}

	data := dashboardData{
		UserSub:   user.Sub,
		UserRole:  role,
		Search:    search,
		ActiveTag: activeTag,
		AllTags:   allTags,
		Apps:      entries,
	}

	// Credential enrollment section
	if srv.Config.Openbao != nil && len(srv.Config.Openbao.Services) > 0 {
		data.Services = buildServiceEntries(srv, user.Sub)
	}

	// Flash messages from credential form redirect
	if errMsg := r.URL.Query().Get("credential_error"); errMsg != "" {
		data.CredentialError = errMsg
	}
	if r.URL.Query().Get("credential_saved") == "1" {
		data.CredentialSuccess = true
	}

	ui.pages["dashboard.html"].ExecuteTemplate(w, "base", data)
}

// --- Builders ---

func buildCatalogEntries(apps []db.AppRow, srv *server.Server) []catalogEntry {
	entries := make([]catalogEntry, 0, len(apps))
	for _, app := range apps {
		tags, _ := srv.DB.ListAppTags(app.ID)
		tagNames := make([]string, len(tags))
		for i, t := range tags {
			tagNames[i] = t.Name
		}

		status := "stopped"
		if srv.Workers.CountForApp(app.ID) > 0 {
			status = "running"
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
				"secret/data/users/"+sub+"/"+svc.Path,
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
