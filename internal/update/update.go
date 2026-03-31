// Package update provides shared logic for checking GitHub releases
// and determining whether a newer version is available.
package update

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// APIBase is the GitHub API base URL for the blockyard repository.
// It is a var (not const) so tests can point it at a local server.
var APIBase = "https://api.github.com/repos/cynkra/blockyard"

// GitHubRelease represents a GitHub release.
type GitHubRelease struct {
	TagName string        `json:"tag_name"`
	Name    string        `json:"name"`
	Assets  []GitHubAsset `json:"assets"`
}

// GitHubAsset represents a downloadable artifact in a release.
type GitHubAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"` // API URL — use with Accept: application/octet-stream
}

// Result holds the outcome of an update check.
type Result struct {
	Channel         string
	CurrentVersion  string
	LatestVersion   string
	UpdateAvailable bool
}

// InferChannel returns "main" or "stable" based on the version string format.
func InferChannel(version string) string {
	if strings.HasPrefix(version, "main+") {
		return "main"
	}
	return "stable"
}

// CheckLatest fetches the latest release for the given channel and compares
// it against currentVersion. Returns a Result indicating whether an update
// is available.
func CheckLatest(channel, currentVersion string) (*Result, error) {
	var rel *GitHubRelease
	var latestVersion string
	var err error

	switch channel {
	case "main":
		rel, err = FetchReleaseByTag("main")
		if err != nil {
			return nil, fmt.Errorf("fetch main release: %w", err)
		}
		latestVersion = rel.Name
	default:
		rel, err = FetchLatestStableRelease()
		if err != nil {
			return nil, fmt.Errorf("fetch latest release: %w", err)
		}
		latestVersion = strings.TrimPrefix(rel.TagName, "v")
	}

	return &Result{
		Channel:         channel,
		CurrentVersion:  currentVersion,
		LatestVersion:   latestVersion,
		UpdateAvailable: latestVersion != currentVersion,
	}, nil
}

// FetchLatestStableRelease fetches the latest tagged (non-prerelease) release.
func FetchLatestStableRelease() (*GitHubRelease, error) {
	return FetchRelease(APIBase + "/releases/latest")
}

// FetchReleaseByTag fetches a release by its Git tag name.
func FetchReleaseByTag(tag string) (*GitHubRelease, error) {
	return FetchRelease(APIBase + "/releases/tags/" + tag)
}

// FetchRelease fetches a single release from the given GitHub API URL.
func FetchRelease(url string) (*GitHubRelease, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	AddGitHubAuth(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var rel GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// AddGitHubAuth sets the Authorization header if GITHUB_TOKEN is set.
func AddGitHubAuth(req *http.Request) {
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}
