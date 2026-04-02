package api

import (
	"encoding/json"
	"net/http"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/server"
)

// GetSystemChecks returns the latest cached system check report.
//
//	@Summary      Get system check report
//	@Description  Returns the latest cached system check report. Admin only.
//	@Tags         system
//	@Produce      json
//	@Success      200  {object}  preflight.Report
//	@Failure      403  {object}  ErrorResponse
//	@Router       /api/v1/system/checks [get]
func GetSystemChecks(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageRoles() {
			forbidden(w, "admin only")
			return
		}

		report := srv.Checker.Latest()
		if report == nil {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"results":[],"summary":{"errors":0,"warnings":0,"info":0,"ok":0}}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(report)
	}
}

// RunSystemChecks triggers a new dynamic check run and returns the full report.
//
//	@Summary      Run system checks
//	@Description  Triggers a new dynamic check run and returns the combined report. Admin only.
//	@Tags         system
//	@Produce      json
//	@Success      200  {object}  preflight.Report
//	@Failure      403  {object}  ErrorResponse
//	@Router       /api/v1/system/checks/run [post]
func RunSystemChecks(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageRoles() {
			forbidden(w, "admin only")
			return
		}

		report := srv.Checker.RunDynamic(r.Context())

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(report)
	}
}
