package api

import (
	"encoding/json"
	"net/http"

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
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid request body")
			return
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

		w.WriteHeader(http.StatusNoContent)
	}
}

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

		w.WriteHeader(http.StatusNoContent)
	}
}
