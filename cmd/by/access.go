package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

func accessCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "access <app> <action>",
		Short: "Manage app access control",
	}

	cmd.AddCommand(
		accessShowCmd(),
		accessSetTypeCmd(),
		accessGrantCmd(),
		accessRevokeCmd(),
	)
	return cmd
}

func accessShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <app>",
		Short: "Show access type and ACL entries",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)
			app := args[0]

			// Get app info for access_type.
			appResp, err := c.get("/api/v1/apps/" + app)
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			var appInfo struct {
				AccessType string `json:"access_type"`
			}
			if err := decodeJSON(appResp, &appInfo); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}

			// Get ACL entries.
			aclResp, err := c.get("/api/v1/apps/" + app + "/access")
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			aclData, err := readBodyRaw(aclResp)
			if err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}

			if jsonOutput {
				var acl any
				_ = json.Unmarshal(aclData, &acl)
				printJSON(map[string]any{
					"access_type": appInfo.AccessType,
					"acl":         acl,
				})
				return nil
			}

			fmt.Printf("Access type: %s\n\n", appInfo.AccessType)

			var entries []struct {
				Principal string `json:"principal"`
				Kind      string `json:"kind"`
				Role      string `json:"role"`
				GrantedBy string `json:"granted_by"`
				GrantedAt string `json:"granted_at"`
			}
			_ = json.Unmarshal(aclData, &entries)

			if len(entries) == 0 {
				fmt.Println("No ACL entries.")
				return nil
			}

			w := newTabWriter()
			fmt.Fprintf(w, "PRINCIPAL\tKIND\tROLE\tGRANTED BY\n")
			for _, e := range entries {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Principal, e.Kind, e.Role, e.GrantedBy)
			}
			_ = w.Flush()
			return nil
		},
	}
}

func accessSetTypeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-type <app> <type>",
		Short: "Set access mode (acl|logged_in|public)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			accessType := args[1]
			valid := map[string]bool{"acl": true, "logged_in": true, "public": true}
			if !valid[accessType] {
				exitErrorf(jsonOutput, "invalid access type %q; must be acl, logged_in, or public", accessType)
			}

			resp, err := c.patchJSON("/api/v1/apps/"+args[0], map[string]string{
				"access_type": accessType,
			})
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			if jsonOutput {
				data, _ := readBodyRaw(resp)
				printRawJSON(data)
				return nil
			}
			if err := checkResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()
			fmt.Printf("Set access type for %s to %s.\n", args[0], accessType)
			return nil
		},
	}
}

func accessGrantCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grant <app> <user>",
		Short: "Grant user access",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			role, _ := cmd.Flags().GetString("role")
			if role == "" {
				role = "viewer"
			}
			valid := map[string]bool{"viewer": true, "collaborator": true}
			if !valid[role] {
				exitErrorf(jsonOutput, "invalid role %q; must be viewer or collaborator", role)
			}

			resp, err := c.postJSON("/api/v1/apps/"+args[0]+"/access", map[string]string{
				"principal": args[1],
				"kind":      "user",
				"role":      role,
			})
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			if err := checkResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()

			if jsonOutput {
				printJSON(map[string]string{
					"status":    "granted",
					"principal": args[1],
					"role":      role,
				})
			} else {
				fmt.Printf("Granted %s access to %s as %s.\n", args[1], args[0], role)
			}
			return nil
		},
	}
	cmd.Flags().String("role", "viewer", "Role to grant (viewer|collaborator)")
	return cmd
}

func accessRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <app> <user>",
		Short: "Revoke user access",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			resp, err := c.delete("/api/v1/apps/" + args[0] + "/access/user/" + args[1])
			if err != nil {
				exitErrorf(jsonOutput, "request failed: %v", err)
			}
			if err := checkResponse(resp); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}
			resp.Body.Close()

			if jsonOutput {
				printJSON(map[string]string{
					"status":    "revoked",
					"principal": args[1],
				})
			} else {
				fmt.Printf("Revoked access for %s on %s.\n", args[1], args[0])
			}
			return nil
		},
	}
}
