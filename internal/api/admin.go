package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/orchestrator"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/task"
)

// handleAdminUpdate triggers a rolling update.
// Returns 202 with a task ID for polling.
func handleAdminUpdate(srv *server.Server, orch *orchestrator.Orchestrator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageRoles() {
			forbidden(w, "admin only")
			return
		}

		if orch == nil {
			writeError(w, http.StatusNotImplemented, "not_implemented",
				"rolling updates require Docker container mode")
			return
		}

		// Parse optional channel override.
		var body struct {
			Channel string `json:"channel"`
		}
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // optional body
		}
		channel := body.Channel
		if channel == "" && srv.Config.Update != nil {
			channel = srv.Config.Update.Channel
		}
		if channel == "" {
			channel = "stable"
		}

		// Concurrency guard — CAS from idle to updating.
		if !orch.CASState("idle", "updating") {
			writeError(w, http.StatusConflict, "conflict",
				"update already in progress (state: "+orch.State()+")")
			return
		}

		taskID := uuid.New().String()
		sender := srv.Tasks.Create(taskID, "admin-update")

		go func() {
			ur, err := orch.Update(r.Context(), channel, sender)
			if err != nil {
				sender.Write(err.Error())
				sender.Complete(task.Failed)
				orch.SetState("idle")
				return
			}
			if ur == nil {
				sender.Complete(task.Completed) // already up to date
				orch.SetState("idle")
				return
			}

			// Enter watchdog mode.
			watchPeriod := srv.Config.Server.DrainTimeout.Duration
			if srv.Config.Update != nil && srv.Config.Update.WatchPeriod.Duration > 0 {
				watchPeriod = srv.Config.Update.WatchPeriod.Duration
			}
			if watchPeriod == 0 {
				watchPeriod = 5 * 60e9 // 5 minutes
			}
			if err := orch.Watchdog(r.Context(), ur.ContainerID, ur.Addr, watchPeriod, sender); err != nil {
				sender.Write(err.Error())
				sender.Complete(task.Failed)
				return // rollback happened, server is still running
			}

			// Watchdog passed — signal the main goroutine to Finish + exit.
			sender.Write("Update successful. Shutting down old server.")
			sender.Complete(task.Completed)
			orch.Exit()
		}()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"task_id": taskID})
	}
}

// handleAdminRollback triggers a rollback to the previous version.
// Returns 202 with a task ID.
func handleAdminRollback(srv *server.Server, orch *orchestrator.Orchestrator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		if caller == nil || !caller.Role.CanManageRoles() {
			forbidden(w, "admin only")
			return
		}

		if orch == nil {
			writeError(w, http.StatusNotImplemented, "not_implemented",
				"rolling updates require Docker container mode")
			return
		}

		// Concurrency guard — CAS from idle to rolling_back.
		if !orch.CASState("idle", "rolling_back") {
			writeError(w, http.StatusConflict, "conflict",
				"operation already in progress (state: "+orch.State()+")")
			return
		}

		taskID := uuid.New().String()
		sender := srv.Tasks.Create(taskID, "admin-rollback")

		go func() {
			err := orch.Rollback(r.Context(), sender, orch.Exit)
			if err != nil {
				sender.Write(err.Error())
				sender.Complete(task.Failed)
				orch.SetState("idle")
				return
			}
			sender.Complete(task.Completed)
			orch.Exit()
		}()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"task_id": taskID})
	}
}

// handleAdminUpdateStatus returns the current update state.
func handleAdminUpdateStatus(orch *orchestrator.Orchestrator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state := "idle"
		if orch != nil {
			state = orch.State()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"state": state,
		})
	}
}

// activationAuth checks if the request carries a valid activation token.
// Used by the activate endpoint for internal orchestrator→new-server calls.
// Falls back to normal admin auth if no activation token is configured.
func activationAuth(srv *server.Server, r *http.Request) bool {
	// Check activation token (set by orchestrator on cloned containers).
	activationToken := os.Getenv("BLOCKYARD_ACTIVATION_TOKEN")
	if activationToken != "" {
		bearer := extractBearerToken(r)
		if bearer != "" && subtle.ConstantTimeCompare(
			[]byte(bearer), []byte(activationToken)) == 1 {
			return true
		}
	}

	// Fall back to normal admin auth.
	caller := auth.CallerFromContext(r.Context())
	return caller != nil && caller.Role.CanManageRoles()
}
