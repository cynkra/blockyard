package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

var apiBase = "https://api.github.com/repos/cynkra/blockyard"

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Name    string        `json:"name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"` // API URL — use with Accept: application/octet-stream
}

func selfUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Update by to the latest release",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			jsonOutput := jsonFlag(cmd)

			channel, _ := cmd.Flags().GetString("channel")
			if channel == "" {
				channel = inferChannel()
			}

			var latest *githubRelease
			var latestVersion string
			var err error

			switch channel {
			case "main":
				latest, err = fetchReleaseByTag("main")
				if err != nil {
					exitError(jsonOutput, fmt.Errorf("fetch main release: %w", err))
				}
				latestVersion = latest.Name
			default:
				latest, err = fetchLatestStableRelease()
				if err != nil {
					exitError(jsonOutput, fmt.Errorf("fetch latest release: %w", err))
				}
				latestVersion = strings.TrimPrefix(latest.TagName, "v")
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

func inferChannel() string {
	if strings.HasPrefix(version, "main+") {
		return "main"
	}
	return "stable"
}

func selfUpdateBinaryName() string {
	name := fmt.Sprintf("by-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func fetchLatestStableRelease() (*githubRelease, error) {
	return fetchRelease(apiBase + "/releases/latest")
}

func fetchReleaseByTag(tag string) (*githubRelease, error) {
	return fetchRelease(apiBase + "/releases/tags/" + tag)
}

func fetchRelease(url string) (*githubRelease, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	addGitHubAuth(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func downloadAsset(apiURL, dst string) error {
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	addGitHubAuth(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %s", resp.Status)
	}

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
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

func addGitHubAuth(req *http.Request) {
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}
