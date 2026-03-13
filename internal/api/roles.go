package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/server"
)

type roleMappingResponse struct {
	GroupName string `json:"group_name"`
	Role      string `json:"role"`
}

func ListRoleMappings(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if !caller.Role.CanManageRoles() {
			forbidden(w, "admin only")
			return
		}

		rows, err := srv.DB.ListRoleMappings()
		if err != nil {
			serverError(w, err.Error())
			return
		}

		resp := make([]roleMappingResponse, len(rows))
		for i, row := range rows {
			resp[i] = roleMappingResponse{
				GroupName: row.GroupName,
				Role:      row.Role,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

type setRoleMappingRequest struct {
	Role string `json:"role"` // "admin" | "publisher" | "viewer"
}

func SetRoleMapping(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if !caller.Role.CanManageRoles() {
			forbidden(w, "admin only")
			return
		}

		groupName := chi.URLParam(r, "group_name")

		var body setRoleMappingRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, "invalid request body")
			return
		}

		role := auth.ParseRole(body.Role)
		if role == auth.RoleNone {
			badRequest(w, "invalid role '"+body.Role+"', must be one of: admin, publisher, viewer")
			return
		}

		if err := srv.DB.UpsertRoleMapping(groupName, body.Role); err != nil {
			serverError(w, err.Error())
			return
		}

		srv.RoleCache.Set(groupName, role)

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionRoleMappingSet, groupName,
				map[string]any{"role": body.Role}))
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func DeleteRoleMapping(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if !caller.Role.CanManageRoles() {
			forbidden(w, "admin only")
			return
		}

		groupName := chi.URLParam(r, "group_name")

		removed, err := srv.DB.DeleteRoleMapping(groupName)
		if err != nil {
			serverError(w, err.Error())
			return
		}

		if !removed {
			notFound(w, "no mapping for group '"+groupName+"'")
			return
		}

		srv.RoleCache.Remove(groupName)

		if srv.AuditLog != nil {
			srv.AuditLog.Emit(auditEntry(r, audit.ActionRoleMappingDelete, groupName, nil))
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
