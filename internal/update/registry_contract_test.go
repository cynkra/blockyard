//go:build registry_contract

// Package update's registry contract test asserts that the install
// target the running code resolves *actually exists* on the GitHub
// Container Registry. Build-tag-gated so it runs only when CI
// explicitly opts in, since it requires network access to ghcr.io
// and only makes sense after a publish has completed.
//
// This is the test that would have caught issue #360: the
// orchestrator was constructing a tag (`:<git-describe>`) that
// publish.yml never pushed. Mock-based tests passed; this contract
// test would have failed at HEAD time with "manifest not found".
//
// Wired into .github/workflows/publish.yml as a step on the manifest
// job — runs after the multi-arch :main manifest is created and
// before the workflow concludes. CI sets GITHUB_TOKEN so the test
// can request a pull-scoped bearer for the public ghcr.io repo;
// anonymous fallback works for forks with the same public visibility.

package update_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/update"
)

// repoFromAPIBase derives "owner/repo" from the configured API base
// (e.g. https://api.github.com/repos/cynkra/blockyard → cynkra/blockyard).
// Used to build the ghcr.io path; ghcr.io/<owner>/<repo> mirrors the
// GitHub repo name.
func repoFromAPIBase() string {
	const marker = "/repos/"
	idx := strings.Index(update.APIBase, marker)
	if idx < 0 {
		return "cynkra/blockyard"
	}
	return update.APIBase[idx+len(marker):]
}

// ghcrToken requests a pull-scoped bearer token for the given
// repository. Uses the GITHUB_TOKEN as basic-auth username when
// available (raises rate limits and works for private repos);
// otherwise falls back to anonymous, which is fine for public ones.
func ghcrToken(ctx context.Context, repo string) (string, error) {
	url := fmt.Sprintf("https://ghcr.io/token?service=ghcr.io&scope=repository:%s:pull", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		// ghcr.io accepts the GH token as the password under any
		// non-empty username for the token-exchange endpoint.
		req.SetBasicAuth("anything", tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ghcr.io token endpoint returned %s", resp.Status)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("ghcr.io token endpoint returned empty token")
	}
	return out.Token, nil
}

// TestMainChannelInstallTargetIsPullable resolves the main-channel
// install target via the production code path and asserts the
// resulting image reference exists on ghcr.io. Failure here is the
// signal we missed in issue #360: code resolves a ref the registry
// doesn't have.
func TestMainChannelInstallTargetIsPullable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	target, err := update.FetchInstallTarget("main", "")
	if err != nil {
		t.Fatalf("FetchInstallTarget(\"main\", \"\"): %v", err)
	}
	if target == "" {
		t.Fatal("FetchInstallTarget returned empty target for main channel; the orchestrator would skip the update entirely")
	}

	repo := repoFromAPIBase()
	token, err := ghcrToken(ctx, repo)
	if err != nil {
		t.Fatalf("get ghcr.io token: %v", err)
	}

	// HEAD the manifest. Accept the OCI manifest+index media types
	// so a multi-arch image (which is what the manifest job pushes)
	// resolves cleanly.
	url := fmt.Sprintf("https://ghcr.io/v2/%s/manifests/%s", repo, target)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ","))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("registry contract violation: ghcr.io/%s:%s does not exist (HEAD returned %s). "+
			"This means the code's resolved install target is not on the registry — "+
			"either the publish workflow didn't push it, or FetchInstallTarget returns "+
			"a tag the workflow never publishes (issue #360).",
			repo, target, resp.Status)
	}
	t.Logf("registry contract OK: ghcr.io/%s:%s exists (digest %s)", repo, target, resp.Header.Get("Docker-Content-Digest"))
}
