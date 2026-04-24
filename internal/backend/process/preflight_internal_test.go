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
// The check's contract since #305 + phase 3-9: host-effective worker
// UIDs require blockyard to run as root so the spawn path can
// fork+setuid(W) before exec(bwrap). When blockyard is root the check
// returns OK; when it is non-root (CI's `unprivileged` matrix, Debian
// 12+/Ubuntu 24.04+ native deployments) the check returns Info
// steering operators toward cgroup-v2 delegation or the Docker
// backend — severity dropped from Error to Info in phase 3-9 because
// the `-m owner` mechanism is inherently inapplicable, not broken.
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
	if result.Severity != preflight.SeverityInfo {
		t.Errorf("non-root blockyard: severity = %v, want Info; message: %s", result.Severity, result.Message)
	}
	// The message must name the non-root mode, point at cgroup-v2
	// delegation, and mention the Docker backend as the alternative
	// path — otherwise operators don't know which remediation fits
	// their deployment.
	for _, want := range []string{"non-root", "cgroup", "Docker backend"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("info message missing %q: %s", want, result.Message)
		}
	}
}
