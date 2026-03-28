package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func tagsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tags",
		Short: "Manage tags (global and per-app)",
		Long: `Manage tags globally or per-app.

Global operations:
  by tags list              List all tags
  by tags create <tag>      Create tag (admin only)
  by tags delete <tag>      Delete tag (admin only)

Per-app operations:
  by tags <app> list        List tags on an app
  by tags <app> add <tag>   Attach tag to app
  by tags <app> remove <tag> Detach tag from app`,
	}

	// Global tag management subcommands.
	cmd.AddCommand(tagsListGlobalCmd())
	cmd.AddCommand(tagsCreateCmd())
	cmd.AddCommand(tagsDeleteGlobalCmd())

	// Per-app tag management: "tags <app> list|add|remove"
	// Cobra can't natively handle a dynamic first arg followed by subcommands,
	// so we register a hidden catch-all.
	cmd.AddCommand(tagsAppListCmd())
	cmd.AddCommand(tagsAppAddCmd())
	cmd.AddCommand(tagsAppRemoveCmd())

	return cmd
}

func tagsListGlobalCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all tags (global pool)",
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)
			return printTagList(c, jsonOutput, "/api/v1/tags")
		},
	}
}

func tagsCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create <tag>",
		Short: "Create a tag (admin only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			resp, err := c.postJSON("/api/v1/tags", map[string]string{"name": args[0]})
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			if jsonOutput {
				data, err := readBodyRaw(resp)
				if err != nil {
					exitErrorf(jsonOutput, "%v", err)
				}
				printRawJSON(data)
				return nil
			}
			if err := checkResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()
			fmt.Printf("Created tag %s.\n", args[0])
			return nil
		},
	}
}

func tagsDeleteGlobalCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <tag>",
		Short: "Delete a tag (admin only, cascades)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			resp, err := c.delete("/api/v1/tags/" + args[0])
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			if err := checkResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()

			if jsonOutput {
				printJSON(map[string]string{"status": "deleted", "tag": args[0]})
			} else {
				fmt.Printf("Deleted tag %s.\n", args[0])
			}
			return nil
		},
	}
}

// Per-app tag subcommands use "app-list", "app-add", "app-remove"
// as hidden cobra commands, aliased from the parent for dispatch.

func tagsAppListCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "app-list <app>",
		Hidden: true,
		Short:  "List tags on an app",
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)
			return printTagList(c, jsonOutput, "/api/v1/apps/"+args[0]+"/tags")
		},
	}
}

func tagsAppAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "app-add <app> <tag>",
		Hidden: true,
		Short:  "Attach tag to app",
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			resp, err := c.postJSON("/api/v1/apps/"+args[0]+"/tags",
				map[string]string{"tag_id": args[1]})
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			if err := checkResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()

			if jsonOutput {
				printJSON(map[string]string{"status": "added", "app": args[0], "tag": args[1]})
			} else {
				fmt.Printf("Added tag %s to %s.\n", args[1], args[0])
			}
			return nil
		},
	}
}

func tagsAppRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "app-remove <app> <tag>",
		Hidden: true,
		Short:  "Detach tag from app",
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			resp, err := c.delete("/api/v1/apps/" + args[0] + "/tags/" + args[1])
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			if err := checkResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()

			if jsonOutput {
				printJSON(map[string]string{"status": "removed", "app": args[0], "tag": args[1]})
			} else {
				fmt.Printf("Removed tag %s from %s.\n", args[1], args[0])
			}
			return nil
		},
	}
}

func printTagList(c *client, jsonOutput bool, path string) error {
	resp, err := c.get(path)
	if err != nil {
		exitErrorf(jsonOutput, "request failed: %v", err)
	}

	if jsonOutput {
		data, err := readBodyRaw(resp)
		if err != nil {
			exitErrorf(jsonOutput, "%v", err)
		}
		printRawJSON(data)
		return nil
	}

	var body struct {
		Tags []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"tags"`
	}
	if err := decodeJSON(resp, &body); err != nil {
		exitErrorf(jsonOutput, "%v", err)
	}

	if len(body.Tags) == 0 {
		fmt.Println("No tags found.")
		return nil
	}

	for _, t := range body.Tags {
		fmt.Println(t.Name)
	}
	return nil
}
