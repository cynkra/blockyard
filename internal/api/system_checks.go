package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/preflight"
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
			report = &preflight.Report{RanAt: time.Now().UTC()}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(report); err != nil {
			slog.Error("failed to encode system checks response", "error", err)
		}
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
		if err := json.NewEncoder(w).Encode(report); err != nil {
			slog.Error("failed to encode system checks response", "error", err)
		}
	}
}
