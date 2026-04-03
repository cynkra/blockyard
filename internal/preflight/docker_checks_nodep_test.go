package preflight

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests for functions in docker_checks.go that do not require a Docker daemon.

func TestCheckHardLink_SameFS(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), ".pkg-store")
	os.MkdirAll(storePath, 0o755)
	res := checkHardLink(storePath)
	if res.Severity != SeverityOK {
		t.Errorf("severity = %v, want OK: %s", res.Severity, res.Message)
	}
	if res.Category != "docker" {
		t.Errorf("category = %q, want docker", res.Category)
	}
}

func TestCheckHardLink_BadStorePath(t *testing.T) {
	// A path inside a read-only location should fail to create .workers dir.
	res := checkHardLink("/proc/nonexistent/.pkg-store")
	if res.Severity != SeverityError {
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
	if res.Severity == SeverityError {
		t.Errorf("unexpected Error severity: %s", res.Message)
	}
}
