//go:build process_test

package process

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/preflight"
)

// TestCheckBwrapHostUIDMapping runs under the process_test tag because
// it needs to call the unexported checkBwrapHostUIDMapping directly. An
// alternative would be to invoke RunPreflight and scan the report for
// the "bwrap_host_uid_mapping" result by name, but direct invocation
// produces clearer failure messages. This test file lives in
// `package process`; the external integration tests stay in
// `package process_test`.
func TestCheckBwrapHostUIDMapping(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not available")
	}
	// Skip if the environment can't actually create user namespaces.
	if err := exec.Command("bwrap",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-pid", "--unshare-user", "--unshare-uts",
		"--die-with-parent", "--new-session",
		"--", "/bin/true").Run(); err != nil {
		t.Skipf("bwrap not functional in this environment: %v", err)
	}

	cfg := &config.ProcessConfig{
		BwrapPath:      "bwrap",
		WorkerUIDStart: 60000,
		WorkerUIDEnd:   60099,
		WorkerGID:      65534,
	}

	result := checkBwrapHostUIDMapping(cfg)

	switch result.Severity {
	case preflight.SeverityOK:
		// Expected when running as root or with setuid bwrap.
	case preflight.SeverityError:
		// Expected with unprivileged bwrap as a non-root caller.
		if !strings.Contains(result.Message, "requested uid") {
			t.Errorf("error message missing requested uid context: %q", result.Message)
		}
	default:
		t.Errorf("unexpected severity %v: %q", result.Severity, result.Message)
	}
}
