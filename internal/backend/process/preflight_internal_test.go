//go:build process_test

package process

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

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
// The test is strict per deployment mode: when bwrap can write a
// host-effective uid_map (mode a or b — root caller or setuid bwrap)
// the check must return OK; when bwrap can spawn but cannot write a
// foreign uid_map (mode c — unprivileged caller without setuid) the
// check MUST return Error and the message must mention the requested
// vs observed UID. Both modes are valid CI configurations and we want
// to catch regressions in either one.
func TestCheckBwrapHostUIDMapping(t *testing.T) {
	mode := probeBwrapModeInternal(t)
	if mode == "unavailable" {
		t.Skip("bwrap not available or unprivileged userns disabled")
	}

	cfg := &config.ProcessConfig{
		BwrapPath:      "bwrap",
		WorkerUIDStart: 60000,
		WorkerUIDEnd:   60099,
		WorkerGID:      65534,
	}

	result := checkBwrapHostUIDMapping(cfg)

	switch mode {
	case "host-mapped":
		if result.Severity != preflight.SeverityOK {
			t.Errorf("mode=host-mapped: severity = %v, want OK; message: %s", result.Severity, result.Message)
		}
	case "no-host-map":
		if result.Severity != preflight.SeverityError {
			t.Errorf("mode=no-host-map: severity = %v, want Error; message: %s", result.Severity, result.Message)
		}
		if !strings.Contains(result.Message, "requested uid") {
			t.Errorf("error message missing requested uid context: %q", result.Message)
		}
	}
}

// probeBwrapModeInternal is the internal-package twin of the
// detectBwrapMode helper in process_integration_test.go. We can't
// share the helper because the integration tests live in
// `package process_test` and this file lives in `package process`.
// Returns "unavailable", "host-mapped", or "no-host-map".
func probeBwrapModeInternal(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		return "unavailable"
	}
	if err := exec.Command("bwrap",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-pid", "--unshare-user", "--unshare-uts",
		"--die-with-parent", "--new-session",
		"--", "/bin/true").Run(); err != nil {
		return "unavailable"
	}
	probeUID := os.Getuid() + 12345
	if probeUID == os.Getuid() {
		probeUID++
	}
	cmd := exec.Command("bwrap",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-pid", "--unshare-user", "--unshare-uts",
		"--uid", strconv.Itoa(probeUID),
		"--gid", "65534",
		"--die-with-parent", "--new-session",
		"--", "/bin/sleep", "0.5")
	if err := cmd.Start(); err != nil {
		return "no-host-map"
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", cmd.Process.Pid))
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "Uid:") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return "no-host-map"
			}
			realUID, perr := strconv.Atoi(fields[1])
			if perr != nil {
				return "no-host-map"
			}
			if realUID == probeUID {
				return "host-mapped"
			}
			return "no-host-map"
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "no-host-map"
}
