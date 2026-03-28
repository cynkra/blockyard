package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func usersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "users",
		Short: "Manage users (admin)",
	}
	cmd.AddCommand(usersListCmd())
	cmd.AddCommand(usersUpdateCmd())
	return cmd
}

func usersListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List users",
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			resp, err := c.get("/api/v1/users")
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

			var users []struct {
				Sub    string `json:"sub"`
				Email  string `json:"email"`
				Name   string `json:"name"`
				Role   string `json:"role"`
				Active bool   `json:"active"`
			}
			if err := decodeJSON(resp, &users); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}

			if len(users) == 0 {
				fmt.Println("No users found.")
				return nil
			}

			w := newTabWriter()
			fmt.Fprintf(w, "SUB\tNAME\tEMAIL\tROLE\tACTIVE\n")
			for _, u := range users {
				active := "yes"
				if !u.Active {
					active = "no"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					u.Sub, u.Name, u.Email, u.Role, active)
			}
			w.Flush()
			return nil
		},
	}
}

func usersUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <sub>",
		Short: "Update user role or active status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			body := make(map[string]any)
			if cmd.Flags().Changed("role") {
				v, _ := cmd.Flags().GetString("role")
				body["role"] = v
			}
			if cmd.Flags().Changed("active") {
				v, _ := cmd.Flags().GetBool("active")
				body["active"] = v
			}

			if len(body) == 0 {
				exitErrorf(jsonOutput, "no flags specified; use --role or --active")
			}

			resp, err := c.patchJSON("/api/v1/users/"+args[0], body)
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
			fmt.Printf("Updated user %s.\n", args[0])
			return nil
		},
	}
	cmd.Flags().String("role", "", "Set role (admin|publisher|viewer)")
	cmd.Flags().Bool("active", true, "Enable/disable user account")
	return cmd
}
