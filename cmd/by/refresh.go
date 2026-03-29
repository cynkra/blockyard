package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cynkra/blockyard/internal/apiclient"
	"github.com/spf13/cobra"
)

func refreshCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh <app>",
		Short: "Refresh unpinned dependencies",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)
			app := args[0]
			rollbackFlag, _ := cmd.Flags().GetBool("rollback")

			var path string
			if rollbackFlag {
				path = "/api/v1/apps/" + app + "/refresh/rollback"
			} else {
				path = "/api/v1/apps/" + app + "/refresh"
			}

			resp, err := c.PostJSON(path, nil)
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}

			var taskResp struct {
				TaskID  string `json:"task_id"`
				Message string `json:"message"`
			}
			if err := apiclient.DecodeJSON(resp, &taskResp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}

			action := "Refreshing"
			if rollbackFlag {
				action = "Rolling back"
			}

			if !jsonOutput {
				fmt.Printf("%s dependencies for %s...\n", action, app)
			}

			// Always stream task logs (refresh is interactive).
			sc := mustStreamingClient(jsonOutput)
			logResp, err := sc.Get(fmt.Sprintf("/api/v1/tasks/%s/logs", taskResp.TaskID))
			if err != nil {
				exitErrorf(jsonOutput, "stream logs: %v", err)
			}
			defer logResp.Body.Close()

			if logResp.StatusCode != 200 {
				body, _ := io.ReadAll(logResp.Body)
				exitErrorf(jsonOutput, "stream logs: HTTP %d: %s", logResp.StatusCode, string(body))
			}

			if jsonOutput {
				var logBuf strings.Builder
				_ = streamResponse(logResp.Body, &logBuf)

				statusResp, _ := c.Get(fmt.Sprintf("/api/v1/tasks/%s", taskResp.TaskID))
				var status struct {
					Status string `json:"status"`
				}
				if statusResp != nil {
					_ = apiclient.DecodeJSON(statusResp, &status)
				}

				printJSON(map[string]any{
					"app":     app,
					"task_id": taskResp.TaskID,
					"status":  status.Status,
				})
				if status.Status == "failed" {
					os.Exit(1)
				}
				return nil
			}

			if err := streamResponse(logResp.Body, os.Stdout); err != nil {
				return fmt.Errorf("stream logs: %w", err)
			}

			// Check final status.
			statusResp, _ := c.Get(fmt.Sprintf("/api/v1/tasks/%s", taskResp.TaskID))
			if statusResp != nil {
				var status struct {
					Status string `json:"status"`
				}
				if apiclient.DecodeJSON(statusResp, &status) == nil && status.Status == "failed" {
					return fmt.Errorf("refresh failed")
				}
			}

			fmt.Println("Done.")
			return nil
		},
	}
	cmd.Flags().Bool("rollback", false, "Roll back to previous dependencies")
	return cmd
}
