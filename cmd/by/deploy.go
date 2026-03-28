package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cynkra/blockyard/internal/manifest"
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
			if !dirExists(absDir) {
				exitErrorf(jsonOutput, "directory not found: %s", dir)
			}

			pinFlag, _ := cmd.Flags().GetBool("pin")
			yesFlag, _ := cmd.Flags().GetBool("yes")
			waitFlag, _ := cmd.Flags().GetBool("wait")
			nameFlag, _ := cmd.Flags().GetString("name")
			reposFlag, _ := cmd.Flags().GetString("repositories")

			c := mustClient(jsonOutput)

			det, warnings := detectApp(absDir, pinFlag)
			if nameFlag != "" {
				det.Name = nameFlag
			}

			for _, w := range warnings {
				if !jsonOutput {
					fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
				}
			}

			// First-deploy confirmation (unless manifest.json already exists or --yes).
			if det.InputCase != caseManifest && !yesFlag && !jsonOutput {
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
			m, err := prepareManifest(absDir, det, reposFlag)
			if err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}

			// Write manifest.json into source dir (for subsequent deploys).
			if m != nil && det.InputCase != caseManifest {
				if err := m.Write(filepath.Join(absDir, "manifest.json")); err != nil {
					exitErrorf(jsonOutput, "write manifest: %v", err)
				}
			}

			// Create tar.gz archive.
			if !jsonOutput {
				fmt.Print("Uploading bundle... ")
			}

			archiveBuf, err := createArchive(absDir)
			if err != nil {
				exitErrorf(jsonOutput, "create archive: %v", err)
			}

			// Ensure app exists on server.
			appID, err := ensureApp(c, det.Name)
			if err != nil {
				exitErrorf(jsonOutput, "%v", err)
			}

			// Upload bundle.
			resp, err := c.post(
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
			if err := decodeJSON(resp, &uploadResp); err != nil {
				exitErrorf(jsonOutput, "upload failed: %v", err)
			}

			serverURL, _, _ := resolveCredentials()
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

// prepareManifest generates a manifest based on the input case.
func prepareManifest(dir string, det *detectResult, reposFlag string) (*manifest.Manifest, error) {
	meta := manifest.Metadata{
		AppMode:    det.Mode,
		Entrypoint: det.Entrypoint,
	}

	switch det.InputCase {
	case caseManifest:
		// 1a: manifest.json exists — validate and use.
		m, err := manifest.Read(filepath.Join(dir, "manifest.json"))
		if err != nil {
			return nil, fmt.Errorf("invalid manifest: %w", err)
		}
		return m, nil

	case caseRenvLock:
		// 1b: renv.lock exists — convert to manifest.
		files := computeFileChecksums(dir)
		m, err := manifest.FromRenvLock(filepath.Join(dir, "renv.lock"), meta, files)
		if err != nil {
			return nil, fmt.Errorf("convert renv.lock: %w", err)
		}
		return m, nil

	case casePinFlag:
		// 1c: --pin — shell out to R + renv, then convert.
		if err := runRenvSnapshot(dir); err != nil {
			return nil, err
		}
		files := computeFileChecksums(dir)
		m, err := manifest.FromRenvLock(filepath.Join(dir, "renv.lock"), meta, files)
		if err != nil {
			return nil, fmt.Errorf("convert renv.lock: %w", err)
		}
		// Clean up renv artifacts if we generated them.
		cleanRenvArtifacts(dir)
		return m, nil

	case caseDescription:
		// 2a: DESCRIPTION exists — build unpinned manifest.
		files := computeFileChecksums(dir)
		repos := parseReposFlag(reposFlag)
		m, err := manifest.FromDescription(filepath.Join(dir, "DESCRIPTION"), meta, files, repos)
		if err != nil {
			return nil, fmt.Errorf("convert DESCRIPTION: %w", err)
		}
		return m, nil

	case caseBareScripts:
		// 2b: bare scripts — no manifest generated, upload as-is.
		return nil, nil

	default:
		return nil, fmt.Errorf("unknown input case")
	}
}

// runRenvSnapshot shells out to R to create an renv.lock file.
func runRenvSnapshot(dir string) error {
	// Check that R is available.
	if _, err := exec.LookPath("Rscript"); err != nil {
		return fmt.Errorf("pinning requires R + renv; Rscript not found in PATH")
	}

	script := `options(renv.consent = TRUE)
deps <- renv::dependencies(".", quiet = TRUE, progress = FALSE)
renv::snapshot(".", packages = deps$Package, prompt = FALSE)`

	cmd := exec.Command("Rscript", "-e", script)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("renv snapshot failed: %w", err)
	}
	return nil
}

// cleanRenvArtifacts removes renv artifacts generated by --pin, unless
// they pre-existed (we check if renv.lock is tracked by looking at the
// directory state before snapshot — but for simplicity, we always clean).
func cleanRenvArtifacts(dir string) {
	_ = os.Remove(filepath.Join(dir, "renv.lock"))
	_ = os.RemoveAll(filepath.Join(dir, "renv"))
	_ = os.Remove(filepath.Join(dir, ".Rprofile"))
}

// parseReposFlag parses the --repositories flag value into Repository entries.
func parseReposFlag(flag string) []manifest.Repository {
	if flag == "" {
		return defaultRepositories()
	}
	var repos []manifest.Repository
	for i, url := range strings.Split(flag, ",") {
		url = strings.TrimSpace(url)
		if url != "" {
			name := fmt.Sprintf("repo%d", i+1)
			repos = append(repos, manifest.Repository{Name: name, URL: url})
		}
	}
	if len(repos) == 0 {
		return defaultRepositories()
	}
	return repos
}

// ensureApp checks if the app exists, and creates it if not.
func ensureApp(c *client, name string) (string, error) {
	// Try to get app by name.
	resp, err := c.get("/api/v1/apps/" + name)
	if err != nil {
		return "", fmt.Errorf("check app: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		var app struct {
			ID string `json:"id"`
		}
		if err := decodeJSON(resp, &app); err != nil {
			return "", fmt.Errorf("read app: %w", err)
		}
		return app.ID, nil
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		return "", fmt.Errorf("unexpected status %d checking app", resp.StatusCode)
	}

	// Create the app.
	createResp, err := c.postJSON("/api/v1/apps", map[string]string{"name": name})
	if err != nil {
		return "", fmt.Errorf("create app: %w", err)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(createResp, &created); err != nil {
		return "", fmt.Errorf("create app: %w", err)
	}
	return created.ID, nil
}

// createArchive creates a tar.gz archive of the directory contents.
func createArchive(dir string) (*os.File, error) {
	tmp, err := os.CreateTemp("", "by-bundle-*.tar.gz")
	if err != nil {
		return nil, err
	}

	gw := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gw)

	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip hidden files/directories.
		if d.Name() != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !d.IsDir() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}

	if err := tw.Close(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	if err := gw.Close(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}

	// Seek to beginning for reading.
	if _, err := tmp.Seek(0, 0); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	return tmp, nil
}

// streamTaskLogs connects to the task log endpoint and streams output.
func streamTaskLogs(c *client, taskID string, jsonOutput bool, appName, bundleID string) error {
	sc := mustStreamingClient(jsonOutput)
	resp, err := sc.get(fmt.Sprintf("/api/v1/tasks/%s/logs", taskID))
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
		statusResp, err := c.get(fmt.Sprintf("/api/v1/tasks/%s", taskID))
		if err != nil {
			return fmt.Errorf("check task status: %w", err)
		}
		var status struct {
			Status string `json:"status"`
		}
		_ = decodeJSON(statusResp, &status)

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
	statusResp, err := c.get(fmt.Sprintf("/api/v1/tasks/%s", taskID))
	if err != nil {
		return nil
	}
	var status struct {
		Status string `json:"status"`
	}
	if decodeJSON(statusResp, &status) == nil && status.Status == "failed" {
		return fmt.Errorf("build failed")
	}

	fmt.Printf("Deployed %s (bundle %s).\n", appName, bundleID)
	return nil
}

// confirm prompts the user with a Y/n question.
func confirm(prompt string) bool {
	fmt.Printf("%s [Y/n] ", prompt)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "" || line == "y" || line == "yes"
}

// dirExists checks if a path is a directory.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
