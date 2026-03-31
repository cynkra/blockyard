package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/task"
)

// GetTaskStatus returns the status of a background task.
//
//	@Summary		Get task status
//	@Description	Returns the current status (running, completed, failed) of a background task.
//	@Tags			tasks
//	@Produce		json
//	@Param			taskID	path		string	true	"Task ID"
//	@Success		200		{object}	taskStatusResponse
//	@Failure		404		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/tasks/{taskID} [get]
func GetTaskStatus(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		taskID := chi.URLParam(r, "taskID")

		status, ok := srv.Tasks.Status(taskID)
		if !ok {
			notFound(w, "task "+taskID+" not found")
			return
		}

		// Verify caller has access to the task's associated app.
		if appID := srv.Tasks.AppID(taskID); appID != "" {
			if _, _, ok := resolveAppRelation(srv, w, caller, appID); !ok {
				return
			}
		}

		statusStr := "running"
		switch status {
		case task.Completed:
			statusStr = "completed"
		case task.Failed:
			statusStr = "failed"
		}

		createdAt := srv.Tasks.CreatedAt(taskID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"id":         taskID,
			"status":     statusStr,
			"created_at": createdAt,
		})
	}
}

// TaskLogs streams the log output of a background task.
//
//	@Summary		Stream task logs
//	@Description	Stream log output for a background task (e.g. bundle restore). Returns buffered output, then follows live lines until task completes.
//	@Tags			tasks
//	@Produce		plain
//	@Param			taskID	path	string	true	"Task ID"
//	@Success		200		"Log output (text/plain, chunked)"
//	@Failure		404		{object}	errorResponse
//	@Security		BearerAuth
//	@Router			/tasks/{taskID}/logs [get]
func TaskLogs(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.CallerFromContext(r.Context())
		taskID := chi.URLParam(r, "taskID")

		status, ok := srv.Tasks.Status(taskID)
		if !ok {
			notFound(w, "task "+taskID+" not found")
			return
		}

		// Verify caller has access to the task's associated app.
		if appID := srv.Tasks.AppID(taskID); appID != "" {
			if _, _, ok := resolveAppRelation(srv, w, caller, appID); !ok {
				return
			}
		}

		snapshot, live, done, ok := srv.Tasks.Subscribe(taskID)
		if !ok {
			notFound(w, "task "+taskID+" not found")
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Transfer-Encoding", "chunked")

		flusher, canFlush := w.(http.Flusher)

		// Write buffered lines
		for _, line := range snapshot {
			fmt.Fprintf(w, "%s\n", line) //nolint:gosec // G705: text/plain SSE stream, not HTML
		}
		if canFlush {
			flusher.Flush()
		}

		// If the task is already done, return the buffer only
		if status != task.Running {
			return
		}

		// Follow live output until task completes or client disconnects.
		// No dedup needed — Subscribe guarantees the live channel only
		// delivers lines written after the snapshot.
		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				// Drain remaining lines from channel
				for {
					select {
					case line, ok := <-live:
						if !ok {
							return
						}
						fmt.Fprintf(w, "%s\n", line)
					default:
						return
					}
				}
			case line, ok := <-live:
				if !ok {
					return
				}
				fmt.Fprintf(w, "%s\n", line)
				if canFlush {
					flusher.Flush()
				}
			}
		}
	}
}
