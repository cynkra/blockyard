package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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
//
//	@Summary		List tags
//	@Description	List all tags, sorted alphabetically.
//	@Tags			tags
//	@Produce		json
//	@Success		200	{array}		tagResponse
//	@Failure		500	{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/tags [get]
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
//
//	@Summary		Create tag
//	@Description	Create a new tag. Admin only. Name must be a lowercase slug.
//	@Tags			tags
//	@Accept			json
//	@Produce		json
//	@Param			body	body		createTagRequest	true	"Tag name"
//	@Success		201		{object}	tagResponse
//	@Failure		400		{object}	errorResponse
//	@Failure		404		{object}	errorResponse
//	@Failure		409		{object}	errorResponse
//	@Failure		500		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/tags [post]
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
//
//	@Summary		Delete tag
//	@Description	Delete a tag. Admin only. Cascades to all app-tag associations.
//	@Tags			tags
//	@Param			tagID	path	string	true	"Tag ID"
//	@Success		204		"Deleted"
//	@Failure		404		{object}	errorResponse
//	@Failure		500		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/tags/{tagID} [delete]
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

		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Trigger", `{"showToast":{"message":"Tag deleted","type":"success"}}`)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type renameTagRequest struct {
	Name string `json:"name"`
}

// RenameTag updates a tag's name. Requires admin/publisher.
//
//	@Summary		Rename tag
//	@Description	Update a tag's name. Requires admin or publisher role.
//	@Tags			tags
//	@Accept			json
//	@Param			tagID	path	string				true	"Tag ID"
//	@Param			body	body	renameTagRequest	true	"New tag name"
//	@Success		200		{object}	tagResponse
//	@Failure		400		{object}	errorResponse
//	@Failure		404		{object}	errorResponse
//	@Failure		409		{object}	errorResponse
//	@Failure		500		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/tags/{tagID} [patch]
func RenameTag(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageTags() {
			notFound(w, "not found")
			return
		}

		tagID := chi.URLParam(r, "tagID")
		tag, err := srv.DB.GetTag(tagID)
		if err != nil {
			serverError(w, "get tag: "+err.Error())
			return
		}
		if tag == nil {
			notFound(w, "tag not found")
			return
		}

		var body renameTagRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid JSON body")
			return
		}
		if body.Name == "" || len(body.Name) > 63 {
			badRequest(w, "name must be 1-63 characters")
			return
		}

		if err := srv.DB.RenameTag(tagID, body.Name); err != nil {
			if db.IsUniqueConstraintError(err) {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(errorResponse{Error: "tag name already exists"})
				return
			}
			serverError(w, "rename tag: "+err.Error())
			return
		}

		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Trigger", `{"showToast":{"message":"Tag renamed","type":"success"}}`)
		}

		tag.Name = body.Name
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tagResp(tag))
	}
}

type addAppTagRequest struct {
	TagID string `json:"tag_id"`
}

// AddAppTag attaches a tag to an app. Requires owner/collaborator/admin.
//
//	@Summary		Add tag to app
//	@Description	Attach a tag to an app. Requires owner, collaborator, or admin role.
//	@Tags			tags
//	@Accept			json
//	@Param			id		path	string			true	"App ID (UUID) or name"
//	@Param			body	body	addAppTagRequest	true	"Tag to attach"
//	@Success		204		"Tag attached"
//	@Failure		400		{object}	errorResponse
//	@Failure		404		{object}	errorResponse
//	@Failure		500		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/tags [post]
func AddAppTag(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		app, relation, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}
		if !relation.CanUpdateConfig() {
			notFound(w, "app not found")
			return
		}

		var body addAppTagRequest
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			_ = r.ParseForm()
			body.TagID = r.FormValue("tag_id")
		} else {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				badRequest(w, "invalid JSON body")
				return
			}
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

		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Trigger", `{"tagAdded":"","showToast":{"message":"Tag added","type":"success"}}`)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// RemoveAppTag detaches a tag from an app. Requires owner/collaborator/admin.
//
//	@Summary		Remove tag from app
//	@Description	Detach a tag from an app. Requires owner, collaborator, or admin role.
//	@Tags			tags
//	@Param			id		path	string	true	"App ID (UUID) or name"
//	@Param			tagID	path	string	true	"Tag ID"
//	@Success		204		"Tag removed"
//	@Failure		404		{object}	errorResponse
//	@Failure		500		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/tags/{tagID} [delete]
func RemoveAppTag(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		id := chi.URLParam(r, "id")

		app, relation, ok := resolveAppRelation(srv, w, caller, id)
		if !ok {
			return
		}
		if !relation.CanUpdateConfig() {
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

		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Trigger", `{"tagRemoved":"","showToast":{"message":"Tag removed","type":"success"}}`)
			w.WriteHeader(http.StatusOK)
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
