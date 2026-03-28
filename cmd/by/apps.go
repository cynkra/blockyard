package main

import (
	"encoding/json"
	"fmt"

	"github.com/cynkra/blockyard/internal/apiclient"
	"github.com/spf13/cobra"
)

func listCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List apps",
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			params := map[string]string{}
			if d, _ := cmd.Flags().GetBool("deleted"); d {
				params["deleted"] = "true"
			}

			resp, err := c.Get(apiclient.BuildQuery("/api/v1/apps", params))
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

			var body struct {
				Apps []struct {
					Name    string  `json:"name"`
					Title   *string `json:"title"`
					Owner   string  `json:"owner"`
					Status  string  `json:"status"`
					Enabled bool    `json:"enabled"`
				} `json:"apps"`
				Total int `json:"total"`
			}
			if err := apiclient.DecodeJSON(resp, &body); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}

			if len(body.Apps) == 0 {
				fmt.Println("No apps found.")
				return nil
			}

			w := newTabWriter()
			fmt.Fprintf(w, "NAME\tTITLE\tOWNER\tSTATUS\tENABLED\n")
			for _, a := range body.Apps {
				title := derefStr(a.Title, "")
				enabled := "yes"
				if !a.Enabled {
					enabled = "no"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					a.Name, truncate(title, 30), a.Owner, a.Status, enabled)
			}
			_ = w.Flush()
			if body.Total > len(body.Apps) {
				fmt.Printf("\nShowing %d of %d apps.\n", len(body.Apps), body.Total)
			}
			return nil
		},
	}
	cmd.Flags().Bool("deleted", false, "Show soft-deleted apps (admin only)")
	return cmd
}

func getCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <app>",
		Short: "Show app details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)
			app := args[0]
			runtimeFlag, _ := cmd.Flags().GetBool("runtime")

			resp, err := c.Get("/api/v1/apps/" + app)
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}

			if jsonOutput {
				appData, err := apiclient.ReadBodyRaw(resp)
				if err != nil {
					exitErrorf(jsonOutput, "%v", err)
				}
				if runtimeFlag {
					runtimeResp, err := c.Get("/api/v1/apps/" + app + "/runtime")
					if err == nil && runtimeResp.StatusCode == 200 {
						runtimeData, _ := apiclient.ReadBodyRaw(runtimeResp)
						// Merge app and runtime into one JSON object.
						var merged map[string]any
						_ = json.Unmarshal(appData, &merged)
						var rt map[string]any
						if json.Unmarshal(runtimeData, &rt) == nil {
							merged["runtime"] = rt
						}
						printJSON(merged)
						return nil
					}
				}
				printRawJSON(appData)
				return nil
			}

			var appInfo struct {
				ID           string   `json:"id"`
				Name         string   `json:"name"`
				Owner        string   `json:"owner"`
				AccessType   string   `json:"access_type"`
				ActiveBundle *string  `json:"active_bundle"`
				MemoryLimit  *string  `json:"memory_limit"`
				CPULimit     *float64 `json:"cpu_limit"`
				Title        *string  `json:"title"`
				Description  *string  `json:"description"`
				Enabled      bool     `json:"enabled"`
				Status       string   `json:"status"`
				Tags         []string `json:"tags"`
				CreatedAt    string   `json:"created_at"`
			}
			if err := apiclient.DecodeJSON(resp, &appInfo); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}

			enabled := "yes"
			if !appInfo.Enabled {
				enabled = "no"
			}

			fmt.Printf("%s\n", appInfo.Name)
			printKeyValue([][2]string{
				{"ID", appInfo.ID},
				{"Owner", appInfo.Owner},
				{"Status", appInfo.Status},
				{"Enabled", enabled},
				{"Access", appInfo.AccessType},
				{"Title", derefStr(appInfo.Title, "(none)")},
				{"Description", derefStr(appInfo.Description, "(none)")},
				{"Bundle", derefStr(appInfo.ActiveBundle, "(none)")},
				{"Memory", derefStr(appInfo.MemoryLimit, "default")},
				{"CPU", derefFloat(appInfo.CPULimit, 0)},
				{"Tags", formatTags(appInfo.Tags)},
				{"Created", appInfo.CreatedAt},
			})

			if runtimeFlag {
				printRuntime(c, app)
			}
			return nil
		},
	}
	cmd.Flags().Bool("runtime", false, "Include live runtime data (workers, sessions, metrics)")
	return cmd
}

func printRuntime(c *apiclient.Client, app string) {
	resp, err := c.Get("/api/v1/apps/" + app + "/runtime")
	if err != nil || resp.StatusCode != 200 {
		// Silently skip if not authorized.
		if resp != nil {
			resp.Body.Close()
		}
		return
	}

	var rt struct {
		Workers []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			StartedAt string `json:"started_at"`
			Stats     struct {
				CPUPercent       float64 `json:"cpu_percent"`
				MemoryUsageBytes uint64  `json:"memory_usage_bytes"`
			} `json:"stats"`
			Sessions []struct {
				ID      string `json:"id"`
				UserSub string `json:"user_sub"`
			} `json:"sessions"`
		} `json:"workers"`
		ActiveSessions int    `json:"active_sessions"`
		TotalViews     int    `json:"total_views"`
		RecentViews    int    `json:"recent_views"`
		UniqueVisitors int    `json:"unique_visitors"`
		LastDeployedAt string `json:"last_deployed_at"`
	}
	if err := apiclient.DecodeJSON(resp, &rt); err != nil {
		return
	}

	fmt.Println()
	fmt.Println("Runtime:")
	printKeyValue([][2]string{
		{"Active sessions", fmt.Sprintf("%d", rt.ActiveSessions)},
		{"Total views", fmt.Sprintf("%d", rt.TotalViews)},
		{"Recent views (7d)", fmt.Sprintf("%d", rt.RecentViews)},
		{"Unique visitors", fmt.Sprintf("%d", rt.UniqueVisitors)},
		{"Last deployed", rt.LastDeployedAt},
	})

	if len(rt.Workers) > 0 {
		fmt.Println()
		w := newTabWriter()
		fmt.Fprintf(w, "  WORKER\tSTATUS\tCPU%%\tMEMORY\tSESSIONS\n")
		for _, wk := range rt.Workers {
			mem := formatBytes(wk.Stats.MemoryUsageBytes)
			fmt.Fprintf(w, "  %s\t%s\t%.1f\t%s\t%d\n",
				truncate(wk.ID, 12), wk.Status, wk.Stats.CPUPercent, mem, len(wk.Sessions))
		}
		_ = w.Flush()
	}
}

func enableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <app>",
		Short: "Enable an app (allow traffic)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)
			resp, err := c.PostJSON("/api/v1/apps/"+args[0]+"/enable", nil)
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			if jsonOutput {
				data, _ := apiclient.ReadBodyRaw(resp)
				printRawJSON(data)
				return nil
			}
			if err := apiclient.CheckResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()
			fmt.Printf("Enabled %s.\n", args[0])
			return nil
		},
	}
}

func disableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <app>",
		Short: "Disable an app (block new traffic, drain sessions)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)
			resp, err := c.PostJSON("/api/v1/apps/"+args[0]+"/disable", nil)
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			if jsonOutput {
				data, _ := apiclient.ReadBodyRaw(resp)
				printRawJSON(data)
				return nil
			}
			if err := apiclient.CheckResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()
			fmt.Printf("Disabled %s.\n", args[0])
			return nil
		},
	}
}

func deleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <app>",
		Short: "Delete an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)
			purge, _ := cmd.Flags().GetBool("purge")

			path := "/api/v1/apps/" + args[0]
			if purge {
				path += "?purge=true"
			}
			resp, err := c.Delete(path)
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			if err := apiclient.CheckResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()

			if jsonOutput {
				action := "deleted"
				if purge {
					action = "purged"
				}
				printJSON(map[string]string{"status": action, "app": args[0]})
			} else if purge {
				fmt.Printf("Purged %s (permanently deleted).\n", args[0])
			} else {
				fmt.Printf("Deleted %s.\n", args[0])
			}
			return nil
		},
	}
	cmd.Flags().Bool("purge", false, "Permanently delete (admin only, must be soft-deleted first)")
	return cmd
}

func restoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <app>",
		Short: "Restore a soft-deleted app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)
			resp, err := c.PostJSON("/api/v1/apps/"+args[0]+"/restore", nil)
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			if jsonOutput {
				data, _ := apiclient.ReadBodyRaw(resp)
				printRawJSON(data)
				return nil
			}
			if err := apiclient.CheckResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()
			fmt.Printf("Restored %s.\n", args[0])
			return nil
		},
	}
}

// formatTags joins tags or returns "(none)".
func formatTags(tags []string) string {
	if len(tags) == 0 {
		return "(none)"
	}
	return fmt.Sprintf("%v", tags)
}

// formatBytes formats bytes as human-readable (e.g., "256 MiB").
func formatBytes(b uint64) string {
	const mib = 1024 * 1024
	const gib = 1024 * mib
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.0f MiB", float64(b)/float64(mib))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

