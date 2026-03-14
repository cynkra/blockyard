package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

type catalogItem struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Title       *string  `json:"title"`
	Description *string  `json:"description"`
	Owner       string   `json:"owner"`
	Tags        []string `json:"tags"`
	Status      string   `json:"status"`
	URL         string   `json:"url"`
	UpdatedAt   string   `json:"updated_at"`
}

// CatalogHandler returns GET /api/v1/catalog — a paginated, RBAC-filtered
// listing of apps with metadata, tags, and search/filter support.
func CatalogHandler(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())

		params := db.CatalogParams{
			Tag:     r.URL.Query().Get("tag"),
			Search:  r.URL.Query().Get("search"),
			Page:    parseIntOr(r.URL.Query().Get("page"), 1),
			PerPage: clamp(parseIntOr(r.URL.Query().Get("per_page"), 20), 1, 100),
		}
		if caller != nil {
			params.CallerSub = caller.Sub
			params.CallerRole = caller.Role.String()
		}

		apps, total, err := srv.DB.ListCatalog(params)
		if err != nil {
			serverError(w, "catalog query failed")
			return
		}

		items := make([]catalogItem, 0, len(apps))
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

			url := "/app/" + app.Name + "/"

			items = append(items, catalogItem{
				ID:          app.ID,
				Name:        app.Name,
				Title:       app.Title,
				Description: app.Description,
				Owner:       app.Owner,
				Tags:        tagNames,
				Status:      status,
				URL:         url,
				UpdatedAt:   app.UpdatedAt,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items":    items,
			"total":    total,
			"page":     params.Page,
			"per_page": params.PerPage,
		})
	}
}

func parseIntOr(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
