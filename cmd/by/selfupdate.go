package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/cynkra/blockyard/internal/update"
	"github.com/spf13/cobra"
)

func selfUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Update by to the latest release",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			jsonOutput := jsonFlag(cmd)

			channel, _ := cmd.Flags().GetString("channel")
			if channel == "" {
				channel = update.InferChannel(version)
			}

			var latest *update.GitHubRelease
			var latestVersion string
			var err error

			switch channel {
			case "main":
				latest, err = update.FetchReleaseByTag("main")
				if err != nil {
					exitError(jsonOutput, fmt.Errorf("fetch main release: %w", err))
				}
				latestVersion = latest.Name
			default:
				latest, err = update.FetchLatestStableRelease()
				if err != nil {
					exitError(jsonOutput, fmt.Errorf("fetch latest release: %w", err))
				}
				latestVersion = latest.TagName
				if len(latestVersion) > 0 && latestVersion[0] == 'v' {
					latestVersion = latestVersion[1:]
				}
			}

			if latestVersion == version {
				if jsonOutput {
					printJSON(map[string]any{
						"current_version": version,
						"latest_version":  latestVersion,
						"channel":         channel,
						"status":          "up_to_date",
					})
				} else {
					fmt.Printf("Already up to date (%s).\n", version)
				}
				return nil
			}

			assetName := selfUpdateBinaryName()
			var assetURL string
			for _, a := range latest.Assets {
				if a.Name == assetName {
					assetURL = a.URL
					break
				}
			}
			if assetURL == "" {
				exitErrorf(jsonOutput, "no release binary found for %s/%s", runtime.GOOS, runtime.GOARCH)
			}

			exe, err := os.Executable()
			if err != nil {
				exitError(jsonOutput, fmt.Errorf("locate current binary: %w", err))
			}
			exe, err = filepath.EvalSymlinks(exe)
			if err != nil {
				exitError(jsonOutput, fmt.Errorf("resolve symlinks: %w", err))
			}

			tmp := exe + ".tmp"
			if err := downloadAsset(assetURL, tmp); err != nil {
				os.Remove(tmp)
				exitError(jsonOutput, fmt.Errorf("download: %w", err))
			}
			if err := os.Rename(tmp, exe); err != nil {
				os.Remove(tmp)
				exitError(jsonOutput, fmt.Errorf("replace binary: %w", err))
			}

			if jsonOutput {
				printJSON(map[string]any{
					"previous_version": version,
					"current_version":  latestVersion,
					"channel":          channel,
					"status":           "updated",
				})
			} else {
				fmt.Printf("Updated %s -> %s\n", version, latestVersion)
			}
			return nil
		},
	}
	cmd.Flags().String("channel", "", `update channel: "stable" or "main" (default: infer from current version)`)
	return cmd
}

func selfUpdateBinaryName() string {
	name := fmt.Sprintf("by-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func downloadAsset(apiURL, dst string) error {
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	update.AddGitHubAuth(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %s", resp.Status)
	}

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) //nolint:gosec // G302: self-update binary needs exec permission
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
