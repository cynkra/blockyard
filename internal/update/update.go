// Package update provides shared logic for checking GitHub releases
// and determining whether the running build is older than the latest
// publicly available version.
//
// The check distinguishes two version shapes:
//
//   - Semver builds (vX.Y.Z, with optional v prefix): compared
//     numerically against the repo's latest stable release.
//   - SHA builds (any string carrying a commit hash — bare hex,
//     git describe, the legacy main+SHA): compared against the
//     origin/main HEAD via the GitHub commits-compare API.
//
// Anything we can't classify (e.g. the literal "dev") is reported as
// State=DevBuild — current is unrecognized, latest release is shown
// for reference but no update is offered.
package update

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// APIBase is the GitHub API root pointing at the configured repo
// (e.g. https://api.github.com/repos/cynkra/blockyard). It is a var
// so SetRepo can rewrite it from config and tests can point it at a
// local httptest server.
var APIBase = "https://api.github.com/repos/cynkra/blockyard"

// SetRepo points subsequent API calls at the given owner/repo
// (e.g. "cynkra/blockyard"). Called once at server startup from
// config so forks can self-host without recompiling.
func SetRepo(repo string) {
	APIBase = "https://api.github.com/repos/" + repo
}

// State enumerates how the running build compares to the upstream
// version the check found.
type State string

const (
	StateUnknown          State = "unknown"           // no check has run yet
	StateUpToDate         State = "up_to_date"        // matches latest release / origin/main HEAD
	StateUpdateAvailable  State = "update_available"  // older than latest release / behind origin/main
	StateAhead            State = "ahead"             // newer than latest release / ahead of origin/main
	StateDiverged         State = "diverged"          // SHA build that diverged from origin/main
	StateDevBuild         State = "dev_build"         // unparseable version string (e.g. "dev")
	StateNoRemote         State = "no_remote"         // GitHub unreachable or returned an error
	StateLocalNotFound    State = "local_not_found"   // SHA known but origin/main doesn't have it
)

// Result is what consumers persist and render. LatestVersion is a
// display string (semver tag, short SHA, or empty when unknown);
// Detail carries human-readable extra context (e.g. "3 commits behind").
type Result struct {
	State          State  `json:"state"`
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// Kind tags the version string format.
type Kind int

const (
	KindUnknown Kind = iota
	KindSemver
	KindSHA
)

var (
	semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)
	// Match a commit hash at the end of the string, optionally
	// followed by -dirty. The hash may stand alone, follow git
	// describe's "g" prefix, or follow the legacy "main+" prefix.
	shaRe = regexp.MustCompile(`(?:^|g|\+)([a-f0-9]{7,40})(-dirty)?$`)
)

// classifyVersion returns the version kind plus, for KindSHA, the
// extracted commit hash (without any -dirty suffix).
func classifyVersion(v string) (Kind, string) {
	if v == "" {
		return KindUnknown, ""
	}
	if semverRe.MatchString(v) {
		return KindSemver, ""
	}
	if m := shaRe.FindStringSubmatch(v); m != nil {
		return KindSHA, m[1]
	}
	return KindUnknown, ""
}

func parseSemver(v string) (tuple [3]int, ok bool) {
	m := semverRe.FindStringSubmatch(v)
	if m == nil {
		return tuple, false
	}
	tuple[0], _ = strconv.Atoi(m[1])
	tuple[1], _ = strconv.Atoi(m[2])
	tuple[2], _ = strconv.Atoi(m[3])
	return tuple, true
}

func compareSemver(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

// shortSHA truncates a hash to 7 chars for display. Strings shorter
// than 7 are returned unchanged.
func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// CheckLatest classifies the current version and queries GitHub to
// produce a Result. Network errors are folded into the Result as
// State=NoRemote rather than returned, so callers can render the
// outcome without an extra error branch — the (error) return is
// reserved for unrecoverable bugs (none today).
func CheckLatest(currentVersion string) (*Result, error) {
	res := &Result{CurrentVersion: currentVersion}
	kind, sha := classifyVersion(currentVersion)

	switch kind {
	case KindSemver:
		rel, err := FetchLatestStableRelease()
		if err != nil {
			res.State = StateNoRemote
			res.Detail = err.Error()
			return res, nil
		}
		latest := strings.TrimPrefix(rel.TagName, "v")
		res.LatestVersion = latest
		cur, _ := parseSemver(currentVersion)
		lat, _ := parseSemver(latest)
		switch compareSemver(cur, lat) {
		case -1:
			res.State = StateUpdateAvailable
		case 0:
			res.State = StateUpToDate
		case 1:
			res.State = StateAhead
			res.Detail = fmt.Sprintf("running %s, latest release is %s", currentVersion, latest)
		}
		return res, nil

	case KindSHA:
		head, err := FetchBranchHEAD("main")
		if err != nil {
			res.State = StateNoRemote
			res.Detail = err.Error()
			return res, nil
		}
		res.LatestVersion = shortSHA(head)
		cmp, err := CompareCommits(sha, head)
		if err != nil {
			res.State = StateNoRemote
			res.Detail = err.Error()
			return res, nil
		}
		if cmp == nil {
			res.State = StateLocalNotFound
			res.Detail = "current commit is not on origin/main (fork or unpushed branch?)"
			return res, nil
		}
		switch cmp.Status {
		case "ahead":
			// head (origin/main) is ahead of base (us) → we are behind.
			res.State = StateUpdateAvailable
			res.Detail = fmt.Sprintf("%d commits behind origin/main", cmp.AheadBy)
		case "behind":
			// head (origin/main) is behind base (us) → we are ahead.
			res.State = StateAhead
			res.Detail = fmt.Sprintf("%d commits ahead of origin/main", cmp.BehindBy)
		case "identical":
			res.State = StateUpToDate
		case "diverged":
			res.State = StateDiverged
			res.Detail = fmt.Sprintf("%d commits ahead, %d commits behind origin/main",
				cmp.BehindBy, cmp.AheadBy)
		default:
			res.State = StateUnknown
			res.Detail = "unexpected compare status: " + cmp.Status
		}
		return res, nil

	default:
		// KindUnknown — e.g. "dev" with no SHA. Show the latest
		// release for reference but do not offer an update.
		res.State = StateDevBuild
		if rel, err := FetchLatestStableRelease(); err == nil {
			res.LatestVersion = strings.TrimPrefix(rel.TagName, "v")
		}
		return res, nil
	}
}

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

// FetchLatestStableRelease fetches the latest tagged (non-prerelease) release.
func FetchLatestStableRelease() (*GitHubRelease, error) {
	return FetchRelease(APIBase + "/releases/latest")
}

// InferChannel picks the default release channel for a build given
// its version string: "stable" when the version is a clean semver
// tag, "main" otherwise. Used by the CLI self-update flow when the
// caller doesn't pass --channel explicitly.
func InferChannel(version string) string {
	kind, _ := classifyVersion(version)
	if kind == KindSemver {
		return "stable"
	}
	return "main"
}

// FetchInstallTarget returns the version string the orchestrator
// should install on the given channel ("stable" or "main"). Returns
// "" when current already matches the channel's head, signalling
// "no update needed". The channel concept is preserved here (vs.
// the version-classifier used by CheckLatest) because operators
// pick which release stream to follow independently of how the
// running build is versioned.
func FetchInstallTarget(channel, currentVersion string) (string, error) {
	var target string
	switch channel {
	case "main":
		rel, err := FetchReleaseByTag("main")
		if err != nil {
			return "", err
		}
		target = rel.Name
	default:
		rel, err := FetchLatestStableRelease()
		if err != nil {
			return "", err
		}
		target = strings.TrimPrefix(rel.TagName, "v")
	}
	if target == currentVersion {
		return "", nil
	}
	return target, nil
}

// FetchReleaseByTag fetches a release by its Git tag name. Used by
// the orchestrator when applying a rolling update.
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

// FetchBranchHEAD returns the HEAD commit SHA of the named branch
// (typically "main"). Used by SHA-build update checks to know what
// the upstream is currently at.
func FetchBranchHEAD(branch string) (string, error) {
	url := APIBase + "/branches/" + branch
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	AddGitHubAuth(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub branches API returned %s", resp.Status)
	}

	var b struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return "", err
	}
	return b.Commit.SHA, nil
}

// CompareResult is the relevant subset of GitHub's compare-commits
// response. Status is one of "ahead", "behind", "identical",
// "diverged"; the counts describe head relative to base.
type CompareResult struct {
	Status   string `json:"status"`
	AheadBy  int    `json:"ahead_by"`
	BehindBy int    `json:"behind_by"`
}

// CompareCommits asks GitHub how head relates to base. Returns
// (nil, nil) when the API responds 404 — typically because base
// isn't reachable on the remote (fork or unpushed branch).
func CompareCommits(base, head string) (*CompareResult, error) {
	url := APIBase + "/compare/" + base + "..." + head
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

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub compare API returned %s", resp.Status)
	}

	var r CompareResult
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// AddGitHubAuth sets the Authorization header if GITHUB_TOKEN is set.
func AddGitHubAuth(req *http.Request) {
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}
