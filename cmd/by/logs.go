package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

func logsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs <app>",
		Short: "Tail app logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			app := args[0]
			workerFlag, _ := cmd.Flags().GetString("worker")
			followFlag, _ := cmd.Flags().GetBool("follow")

			// If no worker specified, auto-select from runtime.
			if workerFlag == "" {
				c := mustClient(jsonOutput)
				wid, note := autoSelectWorker(c, app, jsonOutput)
				if wid == "" {
					exitErrorf(jsonOutput, "no workers found for %s; use --worker to specify", app)
				}
				workerFlag = wid
				if note != "" && !jsonOutput {
					fmt.Fprintln(os.Stderr, note)
				}
			}

			params := map[string]string{
				"worker_id": workerFlag,
			}
			if !followFlag {
				params["stream"] = "false"
			}

			sc := mustStreamingClient(jsonOutput)
			resp, err := sc.get(buildQuery("/api/v1/apps/"+app+"/logs", params))
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != 200 {
				body, _ := io.ReadAll(resp.Body)
				exitErrorf(jsonOutput, "HTTP %d: %s", resp.StatusCode, string(body))
			}

			if jsonOutput {
				data, _ := io.ReadAll(resp.Body)
				printJSON(map[string]any{
					"app":    app,
					"worker": workerFlag,
					"logs":   string(data),
				})
				return nil
			}

			return streamResponse(resp.Body, os.Stdout)
		},
	}
	cmd.Flags().StringP("worker", "w", "", "Worker ID to stream logs from")
	cmd.Flags().BoolP("follow", "f", false, "Stream logs live")
	return cmd
}

// autoSelectWorker picks the most recently started active worker, or falls
// back to the most recently ended worker.
func autoSelectWorker(c *client, app string, jsonOutput bool) (workerID, note string) {
	resp, err := c.get("/api/v1/apps/" + app + "/runtime")
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return "", ""
	}

	var rt struct {
		Workers []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			StartedAt string `json:"started_at"`
		} `json:"workers"`
	}

	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if json.Unmarshal(data, &rt) != nil || len(rt.Workers) == 0 {
		return "", ""
	}

	// Find the most recently started active worker.
	var activeWorkers []struct {
		ID        string
		StartedAt string
	}
	var latestEnded struct {
		ID        string
		StartedAt string
	}

	for _, w := range rt.Workers {
		if w.Status == "active" {
			activeWorkers = append(activeWorkers, struct {
				ID        string
				StartedAt string
			}{w.ID, w.StartedAt})
		}
		if latestEnded.ID == "" || w.StartedAt > latestEnded.StartedAt {
			latestEnded = struct {
				ID        string
				StartedAt string
			}{w.ID, w.StartedAt}
		}
	}

	if len(activeWorkers) > 0 {
		// Pick the most recently started.
		best := activeWorkers[0]
		for _, w := range activeWorkers[1:] {
			if w.StartedAt > best.StartedAt {
				best = w
			}
		}
		if len(activeWorkers) > 1 {
			note = fmt.Sprintf("Streaming worker %s (%d active workers, use --worker to select)",
				truncate(best.ID, 12), len(activeWorkers))
		}
		return best.ID, note
	}

	// Fallback to most recently ended.
	return latestEnded.ID, ""
}
