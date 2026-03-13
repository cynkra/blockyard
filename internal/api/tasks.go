package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/task"
)

func GetTaskStatus(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		taskID := chi.URLParam(r, "taskID")

		status, ok := srv.Tasks.Status(taskID)
		if !ok {
			notFound(w, "task "+taskID+" not found")
			return
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

func TaskLogs(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		taskID := chi.URLParam(r, "taskID")

		status, ok := srv.Tasks.Status(taskID)
		if !ok {
			notFound(w, "task "+taskID+" not found")
			return
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
			fmt.Fprintf(w, "%s\n", line)
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
