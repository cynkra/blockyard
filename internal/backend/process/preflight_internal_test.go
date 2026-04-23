//go:build process_test

package process

import (
	"os"
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
//
// The check's contract since #305: host-effective worker UIDs require
// blockyard to run as root so the spawn path can fork+setuid(W) before
// exec(bwrap), giving bwrap caller_uid == sandbox_uid and therefore an
// identity uid_map. When blockyard is root the check returns OK; when
// it is non-root (CI's `setuid` and `unprivileged` matrices, Debian
// 12+/Ubuntu 24.04+ native deployments) the check returns Error and
// the message must reference the phase-3-9 newuidmap follow-up so
// operators have a remediation path beyond "run as root or use
// Docker".
func TestCheckBwrapHostUIDMapping(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not available")
	}

	cfg := &config.ProcessConfig{
		BwrapPath:      "bwrap",
		WorkerUIDStart: 60000,
		WorkerUIDEnd:   60099,
		WorkerGID:      65534,
	}

	result := checkBwrapHostUIDMapping(cfg)

	if os.Getuid() == 0 {
		if result.Severity != preflight.SeverityOK {
			t.Errorf("root blockyard: severity = %v, want OK; message: %s", result.Severity, result.Message)
		}
		return
	}
	if result.Severity != preflight.SeverityError {
		t.Errorf("non-root blockyard: severity = %v, want Error; message: %s", result.Severity, result.Message)
	}
	// The message must flag the deployment-mode constraint and point
	// at the phase-3-9 remediation, otherwise operators have no
	// actionable next step beyond "Docker backend".
	for _, want := range []string{"root", "phase 3-9"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("error message missing %q: %s", want, result.Message)
		}
	}
}
