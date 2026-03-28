package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/cynkra/blockyard/internal/apiclient"
	"github.com/cynkra/blockyard/internal/cliconfig"
	"github.com/cynkra/blockyard/internal/deploy"
	"github.com/cynkra/blockyard/internal/detect"
	"github.com/spf13/cobra"
)

func deployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy <path>",
		Short: "Deploy a bundle to Blockyard",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOutput := jsonFlag(cmd)
			dir := args[0]

			// Resolve to absolute path.
			absDir, err := filepath.Abs(dir)
			if err != nil {
				exitErrorf(jsonOutput, "invalid path: %v", err)
			}
			if !detect.DirExists(absDir) {
				exitErrorf(jsonOutput, "directory not found: %s", dir)
			}

			pinFlag, _ := cmd.Flags().GetBool("pin")
			yesFlag, _ := cmd.Flags().GetBool("yes")
			waitFlag, _ := cmd.Flags().GetBool("wait")
			nameFlag, _ := cmd.Flags().GetString("name")
			reposFlag, _ := cmd.Flags().GetString("repositories")

			c := mustClient(jsonOutput)

			det, warnings := detect.App(absDir, pinFlag)
			if nameFlag != "" {
				det.Name = nameFlag
			}

			for _, w := range warnings {
				if !jsonOutput {
					fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
				}
			}

			// First-deploy confirmation (unless manifest.json already exists or --yes).
			if det.InputCase != detect.CaseManifest && !yesFlag && !jsonOutput {
				fmt.Println("Detected:")
				printKeyValue([][2]string{
					{"Name", det.Name},
					{"Mode", fmt.Sprintf("%s (entrypoint: %s)", det.Mode, det.Entrypoint)},
					{"Deps", det.DepsLabel},
					{"Repository", det.RepoLabel},
				})
				fmt.Println()
				if !confirm("Deploy?") {
					fmt.Println("Aborted.")
					return nil
				}
			}

			// Generate manifest (if needed).
			m, err := deploy.PrepareManifest(absDir, det, reposFlag)
			if err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}

			// Write manifest.json into source dir (for subsequent deploys).
			if m != nil && det.InputCase != detect.CaseManifest {
				if err := m.Write(filepath.Join(absDir, "manifest.json")); err != nil {
					exitErrorf(jsonOutput, "write manifest: %v", err)
				}
			}

			// Create tar.gz archive.
			if !jsonOutput {
				fmt.Print("Uploading bundle... ")
			}

			archiveBuf, err := deploy.CreateArchive(absDir)
			if err != nil {
				exitErrorf(jsonOutput, "create archive: %v", err)
			}

			// Ensure app exists on server.
			appID, err := ensureApp(c, det.Name)
			if err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}

			// Upload bundle.
			resp, err := c.Post(
				fmt.Sprintf("/api/v1/apps/%s/bundles", appID),
				archiveBuf,
				"application/gzip",
			)
			if err != nil {
				exitErrorf(jsonOutput, "upload failed: %v", err)
			}

			var uploadResp struct {
				BundleID string `json:"bundle_id"`
				TaskID   string `json:"task_id"`
			}
			if err := apiclient.DecodeJSON(resp, &uploadResp); err != nil {
				exitErrorf(jsonOutput, "upload failed: %v", err)
			}

			serverURL, _, _ := cliconfig.ResolveCredentials()
			appURL := serverURL + "/app/" + det.Name + "/"

			if !jsonOutput {
				fmt.Println("done.")
				printKeyValue([][2]string{
					{"App", det.Name},
					{"Bundle", uploadResp.BundleID + " (building)"},
					{"Task", uploadResp.TaskID},
					{"URL", appURL},
				})
			}

			// --wait: stream build logs.
			if waitFlag {
				return streamTaskLogs(c, uploadResp.TaskID, jsonOutput, det.Name, uploadResp.BundleID)
			}

			if jsonOutput {
				printJSON(map[string]any{
					"app":       det.Name,
					"bundle_id": uploadResp.BundleID,
					"task_id":   uploadResp.TaskID,
					"status":    "building",
					"url":       appURL,
				})
			}
			return nil
		},
	}
	cmd.Flags().String("name", "", "Override app name (default: directory basename)")
	cmd.Flags().Bool("pin", false, "Pin dependencies via renv snapshot (requires R + renv)")
	cmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
	cmd.Flags().Bool("wait", false, "Wait for build to complete and stream logs")
	cmd.Flags().String("repositories", "", "Repository URLs (comma-separated)")
	return cmd
}

// ensureApp checks if the app exists, and creates it if not.
func ensureApp(c *apiclient.Client, name string) (string, error) {
	// Try to get app by name.
	resp, err := c.Get("/api/v1/apps/" + name)
	if err != nil {
		return "", fmt.Errorf("check app: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		var app struct {
			ID string `json:"id"`
		}
		if err := apiclient.DecodeJSON(resp, &app); err != nil {
			return "", fmt.Errorf("read app: %w", err)
		}
		return app.ID, nil
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		return "", fmt.Errorf("unexpected status %d checking app", resp.StatusCode)
	}

	// Create the app.
	createResp, err := c.PostJSON("/api/v1/apps", map[string]string{"name": name})
	if err != nil {
		return "", fmt.Errorf("create app: %w", err)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := apiclient.DecodeJSON(createResp, &created); err != nil {
		return "", fmt.Errorf("create app: %w", err)
	}
	return created.ID, nil
}

// streamTaskLogs connects to the task log endpoint and streams output.
func streamTaskLogs(c *apiclient.Client, taskID string, jsonOutput bool, appName, bundleID string) error {
	sc := mustStreamingClient(jsonOutput)
	resp, err := sc.Get(fmt.Sprintf("/api/v1/tasks/%s/logs", taskID))
	if err != nil {
		exitErrorf(jsonOutput, "stream logs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		exitErrorf(jsonOutput, "stream logs: HTTP %d: %s", resp.StatusCode, string(body))
	}

	if !jsonOutput {
		fmt.Println("Building...")
		if err := streamResponse(resp.Body, os.Stdout); err != nil {
			return fmt.Errorf("stream logs: %w", err)
		}
	} else {
		// In JSON mode, consume the log and report final status.
		var logBuf strings.Builder
		_ = streamResponse(resp.Body, &logBuf)

		// Check task status.
		statusResp, err := c.Get(fmt.Sprintf("/api/v1/tasks/%s", taskID))
		if err != nil {
			return fmt.Errorf("check task status: %w", err)
		}
		var status struct {
			Status string `json:"status"`
		}
		_ = apiclient.DecodeJSON(statusResp, &status)

		printJSON(map[string]any{
			"app":       appName,
			"bundle_id": bundleID,
			"task_id":   taskID,
			"status":    status.Status,
		})
		if status.Status == "failed" {
			os.Exit(1)
		}
		return nil
	}

	// Check final task status.
	statusResp, err := c.Get(fmt.Sprintf("/api/v1/tasks/%s", taskID))
	if err != nil {
		return nil
	}
	var status struct {
		Status string `json:"status"`
	}
	if apiclient.DecodeJSON(statusResp, &status) == nil && status.Status == "failed" {
		return fmt.Errorf("build failed")
	}

	fmt.Printf("Deployed %s (bundle %s).\n", appName, bundleID)
	return nil
}

// confirm prompts the user with a Y/n question.
func confirm(prompt string) bool {
	fmt.Printf("%s [Y/n] ", prompt)
	var line string
	fmt.Scanln(&line)
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "" || line == "y" || line == "yes"
}
