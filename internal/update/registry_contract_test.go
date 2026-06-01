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
	"os/exec"
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

// manifestExists HEADs the manifest for ghcr.io/<repo>:<tag> and
// reports whether it resolves. Accepts the OCI manifest+index media
// types so a multi-arch image (what the manifest job pushes) resolves
// cleanly. Returns the resolved digest on success.
func manifestExists(ctx context.Context, repo, tag, token string) (digest string, status string, err error) {
	url := fmt.Sprintf("https://ghcr.io/v2/%s/manifests/%s", repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", "", err
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
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", resp.Status, nil
	}
	return resp.Header.Get("Docker-Content-Digest"), resp.Status, nil
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

	digest, status, err := manifestExists(ctx, repo, target, token)
	if err != nil {
		t.Fatalf("HEAD ghcr.io/%s:%s: %v", repo, target, err)
	}
	if digest == "" {
		t.Fatalf("registry contract violation: ghcr.io/%s:%s does not exist (HEAD returned %s). "+
			"This means the code's resolved install target is not on the registry — "+
			"either the publish workflow didn't push it, or FetchInstallTarget returns "+
			"a tag the workflow never publishes (issue #360).",
			repo, target, status)
	}
	t.Logf("registry contract OK: ghcr.io/%s:%s exists (digest %s)", repo, target, digest)
}

// TestCommitTagIsPullable asserts that the immutable per-commit tag
// (`:<git-describe>`) for the commit under test exists on ghcr.io.
//
// This is the invariant #378 actually broke. The current orchestrator
// pins the main channel to the rolling `:main` tag, so the test above
// can pass while the per-commit tag is missing. But builds published
// before the #360 `:main` fix resolve their install target to the GH
// release Name — which equals `git describe --always --dirty` — and
// such a build is permanently stranded if that tag was never pushed.
// The manifest job must publish `:<VERSION>` alongside `:main`; this
// test fails loudly if it stops doing so.
func TestCommitTagIsPullable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Same command the publish workflow's binary/release/manifest jobs
	// use, so the tag we check matches the one those jobs construct.
	out, err := exec.CommandContext(ctx, "git", "describe", "--always", "--dirty").Output()
	if err != nil {
		t.Fatalf("git describe: %v", err)
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		t.Fatal("git describe returned empty version")
	}

	repo := repoFromAPIBase()
	token, err := ghcrToken(ctx, repo)
	if err != nil {
		t.Fatalf("get ghcr.io token: %v", err)
	}

	digest, status, err := manifestExists(ctx, repo, version, token)
	if err != nil {
		t.Fatalf("HEAD ghcr.io/%s:%s: %v", repo, version, err)
	}
	if digest == "" {
		t.Fatalf("registry contract violation: per-commit tag ghcr.io/%s:%s does not exist (HEAD returned %s). "+
			"The manifest job must push `:<git-describe>` alongside `:main` — without it, any build "+
			"that resolves its install target to the release Name (every build before the #360 :main fix) "+
			"is stranded and can never update itself forward (issue #378).",
			repo, version, status)
	}
	t.Logf("registry contract OK: per-commit tag ghcr.io/%s:%s exists (digest %s)", repo, version, digest)
}
