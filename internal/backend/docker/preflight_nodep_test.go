package docker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/preflight"
)

// Tests for functions in preflight.go that do not require a Docker daemon.

func TestCheckHardLink_SameFS(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), ".pkg-store")
	os.MkdirAll(storePath, 0o755)
	res := checkHardLink(storePath)
	if res.Severity != preflight.SeverityOK {
		t.Errorf("severity = %v, want OK: %s", res.Severity, res.Message)
	}
	if res.Category != "docker" {
		t.Errorf("category = %q, want docker", res.Category)
	}
}

func TestCheckHardLink_BadStorePath(t *testing.T) {
	// A path inside a read-only location should fail to create .workers dir.
	res := checkHardLink("/proc/nonexistent/.pkg-store")
	if res.Severity != preflight.SeverityError {
		t.Errorf("severity = %v, want Error for bad path: %s", res.Severity, res.Message)
	}
}

func TestCheckMetadataBlocking(t *testing.T) {
	// Without a serverID, the function probes iptables (likely unavailable
	// in test environments). Just verify it returns a valid result.
	res := checkMetadataBlocking("")
	if res.Name != "metadata_endpoint" {
		t.Errorf("name = %q, want metadata_endpoint", res.Name)
	}
	if res.Category != "docker" {
		t.Errorf("category = %q, want docker", res.Category)
	}
}

func TestCheckMetadataBlocking_WithServerID(t *testing.T) {
	// With a non-empty serverID, the function tries a TCP connect to
	// 169.254.169.254:80 (link-local metadata). In test environments
	// this is unreachable, yielding OK.
	res := checkMetadataBlocking("fake-container-id")
	if res.Name != "metadata_endpoint" {
		t.Errorf("name = %q, want metadata_endpoint", res.Name)
	}
	// Accept OK or Warning — depends on whether metadata is reachable
	// and iptables is available.
	if res.Severity == preflight.SeverityError {
		t.Errorf("unexpected Error severity: %s", res.Message)
	}
}

// TestCheckRedisOnServiceNetwork_MissingConfig hits the early OK-return
// branch when RedisURL is empty — the check is skipped with a "not
// configured" message. This does not require a Docker client.
func TestCheckRedisOnServiceNetwork_MissingConfig(t *testing.T) {
	d := &DockerBackend{config: &config.DockerConfig{}}
	res := checkRedisOnServiceNetwork(context.Background(), d, PreflightDeps{RedisURL: ""})
	if res.Severity != preflight.SeverityOK {
		t.Errorf("severity = %v, want OK when unconfigured", res.Severity)
	}
}

// TestCheckRedisOnServiceNetwork_ParseError hits the "parse error"
// branch when the URL is malformed — the function returns OK with a
// skip message rather than an error.
func TestCheckRedisOnServiceNetwork_ParseError(t *testing.T) {
	d := &DockerBackend{config: &config.DockerConfig{ServiceNetwork: "svc-net"}}
	res := checkRedisOnServiceNetwork(context.Background(), d,
		PreflightDeps{RedisURL: "::::not-a-url"})
	if res.Severity != preflight.SeverityOK {
		t.Errorf("severity = %v, want OK on parse error", res.Severity)
	}
	if res.Category != "docker" {
		t.Errorf("category = %q", res.Category)
	}
}

// TestCheckRedisOnServiceNetwork_EmptyHost hits the "no host" branch
// where a URL parses but has no hostname (e.g. scheme-only).
func TestCheckRedisOnServiceNetwork_EmptyHost(t *testing.T) {
	d := &DockerBackend{config: &config.DockerConfig{ServiceNetwork: "svc-net"}}
	res := checkRedisOnServiceNetwork(context.Background(), d,
		PreflightDeps{RedisURL: "redis://"})
	if res.Severity != preflight.SeverityOK {
		t.Errorf("severity = %v, want OK when URL has no host", res.Severity)
	}
}

// TestCleanupOrphanResources_NoOpReturnsNil exercises the trivial
// public CleanupOrphanResources method, covering the 0% block.
func TestCleanupOrphanResources_NoOpReturnsNil(t *testing.T) {
	d := &DockerBackend{
		runCmd: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return nil, nil
		},
	}
	if err := d.CleanupOrphanResources(context.Background()); err != nil {
		t.Errorf("CleanupOrphanResources = %v, want nil", err)
	}
}

// TestCheckRVersion_AlwaysNil covers the trivial CheckRVersion method
// on DockerBackend — the docker backend selects R via image tag and
// always returns nil.
func TestCheckRVersion_AlwaysNil(t *testing.T) {
	d := &DockerBackend{}
	if err := d.CheckRVersion("4.5.0"); err != nil {
		t.Errorf("CheckRVersion = %v, want nil", err)
	}
	if err := d.CheckRVersion(""); err != nil {
		t.Errorf("CheckRVersion(\"\") = %v, want nil", err)
	}
}
