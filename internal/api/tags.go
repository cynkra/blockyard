package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
)

type tagResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

func tagResp(t *db.TagRow) tagResponse {
	return tagResponse{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt}
}

// ListTags returns all tags, sorted by name.
func ListTags(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tags, err := srv.DB.ListTags()
		if err != nil {
			serverError(w, "list tags: "+err.Error())
			return
		}

		resp := make([]tagResponse, 0, len(tags))
		for _, t := range tags {
			resp = append(resp, tagResponse{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

type createTagRequest struct {
	Name string `json:"name"`
}

// CreateTag creates a new tag (admin only).
func CreateTag(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageTags() {
			notFound(w, "not found")
			return
		}

		var body createTagRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid JSON body")
			return
		}

		if err := validateTagName(body.Name); err != nil {
			badRequest(w, err.Error())
			return
		}

		tag, err := srv.DB.CreateTag(body.Name)
		if err != nil {
			if db.IsUniqueConstraintError(err) {
				conflict(w, fmt.Sprintf("tag %q already exists", body.Name))
				return
			}
			serverError(w, "create tag: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(tagResp(tag))
	}
}

// DeleteTag deletes a tag by ID (admin only). Cascades to app_tags.
func DeleteTag(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageTags() {
			notFound(w, "not found")
			return
		}

		tagID := chi.URLParam(r, "tagID")
		deleted, err := srv.DB.DeleteTag(tagID)
		if err != nil {
			serverError(w, "delete tag: "+err.Error())
			return
		}
		if !deleted {
			notFound(w, "tag not found")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

type addAppTagRequest struct {
	TagID string `json:"tag_id"`
}

// AddAppTag attaches a tag to an app. Requires owner/collaborator/admin.
func AddAppTag(srv *server.Server) http.HandlerFunc {
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

		var body addAppTagRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid JSON body")
			return
		}
		if body.TagID == "" {
			badRequest(w, "tag_id is required")
			return
		}

		// Verify tag exists
		tag, err := srv.DB.GetTag(body.TagID)
		if err != nil {
			serverError(w, "get tag: "+err.Error())
			return
		}
		if tag == nil {
			notFound(w, "tag not found")
			return
		}

		if err := srv.DB.AddAppTag(app.ID, body.TagID); err != nil {
			serverError(w, "add app tag: "+err.Error())
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// RemoveAppTag detaches a tag from an app. Requires owner/collaborator/admin.
func RemoveAppTag(srv *server.Server) http.HandlerFunc {
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

		tagID := chi.URLParam(r, "tagID")
		deleted, err := srv.DB.RemoveAppTag(app.ID, tagID)
		if err != nil {
			serverError(w, "remove app tag: "+err.Error())
			return
		}
		if !deleted {
			notFound(w, "tag association not found")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// validateTagName checks that a tag name is valid.
// Same rules as app names: 1-63 lowercase ASCII letters, digits, hyphens,
// starting with a letter, not ending with a hyphen.
func validateTagName(name string) error {
	if len(name) == 0 || len(name) > 63 {
		return fmt.Errorf("tag name must be 1-63 characters")
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return fmt.Errorf("tag name must contain only lowercase letters, digits, and hyphens")
		}
	}
	if name[0] < 'a' || name[0] > 'z' {
		return fmt.Errorf("tag name must start with a lowercase letter")
	}
	if name[len(name)-1] == '-' {
		return fmt.Errorf("tag name must not end with a hyphen")
	}
	return nil
}
