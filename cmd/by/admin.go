package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cynkra/blockyard/internal/apiclient"
)

func adminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Server administration commands",
		Long:  "Commands that manage the blockyard server itself. Requires admin role.",
	}
	cmd.AddCommand(
		adminUpdateCmd(),
		adminRollbackCmd(),
		adminStatusCmd(),
	)
	return cmd
}

func adminUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Trigger a rolling update of the server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			channel, _ := cmd.Flags().GetString("channel")
			yes, _ := cmd.Flags().GetBool("yes")

			// Pre-flight: check what's available.
			resp, err := c.Get("/api/v1/admin/update/status")
			if err != nil {
				exitError(jsonOutput, err)
			}
			var status updateStatus
			if err := apiclient.DecodeJSON(resp, &status); err != nil {
				exitError(jsonOutput, err)
			}
			if status.State != "idle" {
				exitErrorf(jsonOutput,
					"update already in progress (state: %s)", status.State)
			}

			// Confirmation prompt.
			if !yes && !jsonOutput {
				ch := channel
				if ch == "" {
					ch = "stable"
				}
				fmt.Printf("Update server to latest %s release? [y/N] ", ch)
				var answer string
				fmt.Scanln(&answer)
				if answer != "y" && answer != "Y" {
					fmt.Println("Cancelled.")
					return nil
				}
			}

			// Trigger update.
			body := map[string]any{}
			if channel != "" {
				body["channel"] = channel
			}
			resp, err = c.PostJSON("/api/v1/admin/update", body)
			if err != nil {
				exitError(jsonOutput, err)
			}
			var result struct{ TaskID string `json:"task_id"` }
			if err := apiclient.DecodeJSON(resp, &result); err != nil {
				exitError(jsonOutput, err)
			}

			if jsonOutput {
				printJSON(result)
				return nil
			}

			// Stream progress.
			return streamAdminTaskProgress(c, result.TaskID)
		},
	}
	cmd.Flags().String("channel", "",
		`update channel: "stable" or "main" (default: server config)`)
	cmd.Flags().Bool("yes", false, "skip confirmation prompt")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func adminRollbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Roll back the server to the previous version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			yes, _ := cmd.Flags().GetBool("yes")

			// Confirmation prompt.
			if !yes && !jsonOutput {
				fmt.Print("Roll back server to previous version? [y/N] ")
				var answer string
				fmt.Scanln(&answer)
				if answer != "y" && answer != "Y" {
					fmt.Println("Cancelled.")
					return nil
				}
			}

			resp, err := c.PostJSON("/api/v1/admin/rollback", nil)
			if err != nil {
				exitError(jsonOutput, err)
			}
			var result struct{ TaskID string `json:"task_id"` }
			if err := apiclient.DecodeJSON(resp, &result); err != nil {
				exitError(jsonOutput, err)
			}

			if jsonOutput {
				printJSON(result)
				return nil
			}

			return streamAdminTaskProgress(c, result.TaskID)
		},
	}
	cmd.Flags().Bool("yes", false, "skip confirmation prompt")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func adminStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show current update state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)
			resp, err := c.Get("/api/v1/admin/update/status")
			if err != nil {
				exitError(jsonOutput, err)
			}
			var status updateStatus
			if err := apiclient.DecodeJSON(resp, &status); err != nil {
				exitError(jsonOutput, err)
			}
			if jsonOutput {
				printJSON(status)
			} else {
				fmt.Printf("State:   %s\n", status.State)
				if status.Version != "" {
					fmt.Printf("Version: %s\n", status.Version)
				}
				if status.Message != "" {
					fmt.Printf("Message: %s\n", status.Message)
				}
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

type updateStatus struct {
	State   string `json:"state"`
	TaskID  string `json:"task_id,omitempty"`
	Version string `json:"version,omitempty"`
	Message string `json:"message,omitempty"`
}

// streamAdminTaskProgress polls the task log endpoint and prints
// incremental output lines. Same pattern used by deploy for build progress.
func streamAdminTaskProgress(c *apiclient.Client, taskID string) error {
	sc := mustStreamingClient(false)
	resp, err := sc.Get(fmt.Sprintf("/api/v1/tasks/%s/logs", taskID))
	if err != nil {
		return fmt.Errorf("stream logs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stream logs: HTTP %d: %s", resp.StatusCode, string(body))
	}

	if err := streamResponse(resp.Body, os.Stdout); err != nil {
		return fmt.Errorf("stream logs: %w", err)
	}

	// Check final task status.
	statusResp, err := c.Get(fmt.Sprintf("/api/v1/tasks/%s", taskID))
	if err != nil {
		return nil
	}
	var status struct {
		Status string `json:"status"`
	}
	if apiclient.DecodeJSON(statusResp, &status) == nil &&
		strings.ToLower(status.Status) == "failed" {
		return fmt.Errorf("operation failed")
	}

	return nil
}
