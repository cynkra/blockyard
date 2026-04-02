package api

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/server"
)

// activateHandler starts background goroutines on a passive server.
// Returns 200 on success, 403 if not admin, 409 if already active or
// not passive.
//
// The sync.Once is scoped to the closure — no exported field on Server
// needed, and the once-guard lives where it's used.
func activateHandler(srv *server.Server, startBG func()) http.HandlerFunc {
	var once sync.Once
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageRoles() {
			forbidden(w, "admin only")
			return
		}

		if !srv.Passive.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "server is already active",
			})
			return
		}

		activated := false
		once.Do(func() {
			startBG()
			srv.Passive.Store(false)
			activated = true
		})

		w.Header().Set("Content-Type", "application/json")
		if activated {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"status": "activated",
			})
		} else {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "activation already in progress",
			})
		}
	}
}
