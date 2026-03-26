package api

import (
	"encoding/json"
	"net/http"

	"github.com/cynkra/blockyard/internal/server"
)

// PostPackages handles runtime package installation requests from workers.
func PostPackages(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		workerID := WorkerIDFromContext(r.Context())
		appID := AppIDFromContext(r.Context())

		var req server.PackageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest,
				server.PackageResponse{Status: "error", Message: "invalid request"})
			return
		}

		result, err := srv.InstallPackage(r.Context(), appID, workerID, req)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError,
				server.PackageResponse{Status: "error", Message: err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, result)
	}
}
