package main

import (
	"fmt"

	"github.com/cynkra/blockyard/internal/apiclient"
	"github.com/spf13/cobra"
)

func scaleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scale <app>",
		Short: "Configure resource limits and scaling",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			body := make(map[string]any)
			if cmd.Flags().Changed("memory") {
				v, _ := cmd.Flags().GetString("memory")
				body["memory_limit"] = v
			}
			if cmd.Flags().Changed("cpu") {
				v, _ := cmd.Flags().GetFloat64("cpu")
				body["cpu_limit"] = v
			}
			if cmd.Flags().Changed("max-workers") {
				v, _ := cmd.Flags().GetInt("max-workers")
				body["max_workers_per_app"] = v
			}
			if cmd.Flags().Changed("max-sessions") {
				v, _ := cmd.Flags().GetInt("max-sessions")
				body["max_sessions_per_worker"] = v
			}
			if cmd.Flags().Changed("pre-warm") {
				v, _ := cmd.Flags().GetInt("pre-warm")
				body["pre_warmed_seats"] = v
			}

			if len(body) == 0 {
				exitErrorf(jsonOutput, "no flags specified; use --memory, --cpu, --max-workers, --max-sessions, or --pre-warm")
			}

			resp, err := c.PatchJSON("/api/v1/apps/"+args[0], body)
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}

			if jsonOutput {
				data, err := apiclient.ReadBodyRaw(resp)
				if err != nil {
					exitErrorf(jsonOutput, "%v", err)
				}
				printRawJSON(data)
				return nil
			}

			if err := apiclient.CheckResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()
			fmt.Printf("Updated scaling for %s.\n", args[0])
			return nil
		},
	}
	cmd.Flags().String("memory", "", "Memory limit (e.g., \"2g\")")
	cmd.Flags().Float64("cpu", 0, "CPU limit")
	cmd.Flags().Int("max-workers", 0, "Max workers per app")
	cmd.Flags().Int("max-sessions", 0, "Max sessions per worker")
	cmd.Flags().Int("pre-warm", 0, "Pre-warmed standby workers")
	return cmd
}

func updateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <app>",
		Short: "Update app metadata",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			body := make(map[string]any)
			if cmd.Flags().Changed("title") {
				v, _ := cmd.Flags().GetString("title")
				body["title"] = v
			}
			if cmd.Flags().Changed("description") {
				v, _ := cmd.Flags().GetString("description")
				body["description"] = v
			}

			if len(body) == 0 {
				exitErrorf(jsonOutput, "no flags specified; use --title or --description")
			}

			resp, err := c.PatchJSON("/api/v1/apps/"+args[0], body)
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}

			if jsonOutput {
				data, err := apiclient.ReadBodyRaw(resp)
				if err != nil {
					exitErrorf(jsonOutput, "%v", err)
				}
				printRawJSON(data)
				return nil
			}

			if err := apiclient.CheckResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()
			fmt.Printf("Updated %s.\n", args[0])
			return nil
		},
	}
	cmd.Flags().String("title", "", "Display title")
	cmd.Flags().String("description", "", "Description")
	return cmd
}
