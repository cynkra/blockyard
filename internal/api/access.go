package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/authz"
	"github.com/cynkra/blockyard/internal/server"
)

type grantRequest struct {
	Principal string `json:"principal"`
	Kind      string `json:"kind"` // "user"
	Role      string `json:"role"` // "viewer" | "collaborator"
}

type accessGrantResponse struct {
	Principal string `json:"principal"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	GrantedBy string `json:"granted_by"`
	GrantedAt string `json:"granted_at"`
}

// GrantAccess grants access to an app for a user.
//
//	@Summary		Grant access
//	@Description	Grant a user viewer or collaborator access to an app. Requires owner or admin role.
//	@Tags			access
//	@Accept			json
//	@Param			id		path	string			true	"App ID (UUID) or name"
//	@Param			body	body	grantRequest	true	"Access grant"
//	@Success		204		"Access granted"
//	@Failure		400		{object}	errorResponse
//	@Failure		404		{object}	errorResponse
//	@Failure		500		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/access [post]
func GrantAccess(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		appID := chi.URLParam(r, "id")

		app, relation, ok := resolveAppRelation(srv, w, caller, appID)
		if !ok {
			return
		}

		if !relation.CanManageACL() {
			notFound(w, "app not found")
			return
		}

		var body grantRequest
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			_ = r.ParseForm()
			body.Principal = r.FormValue("principal")
			body.Kind = r.FormValue("kind")
			body.Role = r.FormValue("role")
		} else {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				badRequest(w, "invalid request body")
				return
			}
		}

		if body.Kind != "user" {
			badRequest(w, "kind must be 'user'")
			return
		}

		if _, valid := authz.ParseContentRole(body.Role); !valid {
			badRequest(w, "role must be 'viewer' or 'collaborator'")
			return
		}

		if body.Principal == "" {
			badRequest(w, "principal must not be empty")
			return
		}

		if body.Kind == "user" && body.Principal == caller.Sub {
			badRequest(w, "cannot grant access to yourself")
			return
		}

		if err := srv.DB.GrantAppAccess(
			app.ID, body.Principal, body.Kind, body.Role, caller.Sub,
		); err != nil {
			serverError(w, err.Error())
			return
		}

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionAccessGrant, app.ID,
				map[string]any{"principal": body.Principal, "role": body.Role}))
		}

		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Trigger", `{"showToast":{"message":"Access granted","type":"success"}}`)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ListAccess lists all access grants for an app.
//
//	@Summary		List access grants
//	@Description	List all ACL entries for an app. Requires owner or admin role.
//	@Tags			access
//	@Produce		json
//	@Param			id	path		string	true	"App ID (UUID) or name"
//	@Success		200	{array}		accessGrantResponse
//	@Failure		404	{object}	errorResponse
//	@Failure		500	{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/access [get]
func ListAccess(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		appID := chi.URLParam(r, "id")

		app, relation, ok := resolveAppRelation(srv, w, caller, appID)
		if !ok {
			return
		}

		if !relation.CanManageACL() {
			notFound(w, "app not found")
			return
		}

		rows, err := srv.DB.ListAppAccess(app.ID)
		if err != nil {
			serverError(w, err.Error())
			return
		}

		resp := make([]accessGrantResponse, len(rows))
		for i, row := range rows {
			resp[i] = accessGrantResponse{
				Principal: row.Principal,
				Kind:      row.Kind,
				Role:      row.Role,
				GrantedBy: row.GrantedBy,
				GrantedAt: row.GrantedAt,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// RevokeAccess revokes a user's access to an app.
//
//	@Summary		Revoke access
//	@Description	Remove a specific access grant from an app. Requires owner or admin role.
//	@Tags			access
//	@Param			id			path	string	true	"App ID (UUID) or name"
//	@Param			kind		path	string	true	"Grant kind (e.g. 'user')"
//	@Param			principal	path	string	true	"User sub"
//	@Success		204			"Access revoked"
//	@Failure		404			{object}	errorResponse
//	@Failure		500			{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/apps/{id}/access/{kind}/{principal} [delete]
func RevokeAccess(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		appID := chi.URLParam(r, "id")
		kind := chi.URLParam(r, "kind")
		principal := chi.URLParam(r, "principal")

		_, relation, ok := resolveAppRelation(srv, w, caller, appID)
		if !ok {
			return
		}

		if !relation.CanManageACL() {
			notFound(w, "app not found")
			return
		}

		removed, err := srv.DB.RevokeAppAccess(appID, principal, kind)
		if err != nil {
			serverError(w, err.Error())
			return
		}

		if !removed {
			notFound(w, "grant not found")
			return
		}

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionAccessRevoke, appID,
				map[string]any{"principal": principal}))
		}

		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Trigger", `{"showToast":{"message":"Access revoked","type":"success"}}`)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
