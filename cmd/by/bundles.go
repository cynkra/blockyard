package main

import (
	"fmt"

	"github.com/cynkra/blockyard/internal/apiclient"
	"github.com/spf13/cobra"
)

func bundlesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bundles <app>",
		Short: "List bundles for an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			resp, err := c.Get("/api/v1/apps/" + args[0] + "/bundles")
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
				Bundles []struct {
					ID         string  `json:"id"`
					Status     string  `json:"status"`
					UploadedAt string  `json:"uploaded_at"`
					DeployedBy *string `json:"deployed_by"`
					Pinned     bool    `json:"pinned"`
				} `json:"bundles"`
			}
			if err := apiclient.DecodeJSON(resp, &body); err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}

			if len(body.Bundles) == 0 {
				fmt.Println("No bundles found.")
				return nil
			}

			w := newTabWriter()
			fmt.Fprintf(w, "ID\tSTATUS\tUPLOADED\tDEPLOYED BY\tPINNED\n")
			for _, b := range body.Bundles {
				pinned := "no"
				if b.Pinned {
					pinned = "yes"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					truncate(b.ID, 12), b.Status, b.UploadedAt,
					derefStr(b.DeployedBy, "-"), pinned)
			}
			_ = w.Flush()
			return nil
		},
	}
}

func rollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback <app> <bundle-id>",
		Short: "Roll back to a previous bundle",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			c := mustClient(jsonOutput)

			resp, err := c.PostJSON("/api/v1/apps/"+args[0]+"/rollback",
				map[string]string{"bundle_id": args[1]})
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
			fmt.Printf("Rolled back %s to bundle %s.\n", args[0], args[1])
			return nil
		},
	}
}
