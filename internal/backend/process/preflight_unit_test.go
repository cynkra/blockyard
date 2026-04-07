package process

import (
	"os"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/preflight"
)

func TestCheckBwrapMissing(t *testing.T) {
	res := checkBwrap(&config.ProcessConfig{BwrapPath: "/nonexistent/bwrap"})
	if res.Severity != preflight.SeverityError {
		t.Errorf("severity = %v, want Error", res.Severity)
	}
	if res.Name != "bwrap_available" {
		t.Errorf("name = %q, want bwrap_available", res.Name)
	}
	if res.Category != "process" {
		t.Errorf("category = %q, want process", res.Category)
	}
}

func TestCheckRBinaryMissing(t *testing.T) {
	res := checkRBinary(&config.ProcessConfig{RPath: "/nonexistent/R"})
	if res.Severity != preflight.SeverityError {
		t.Errorf("severity = %v, want Error", res.Severity)
	}
}

func TestCheckRBinaryPresent(t *testing.T) {
	// /bin/sh is guaranteed on POSIX systems and stands in for R here;
	// checkRBinary only does LookPath, not an R-specific invocation.
	res := checkRBinary(&config.ProcessConfig{RPath: "/bin/sh"})
	if res.Severity != preflight.SeverityOK {
		t.Errorf("severity = %v, want OK: %s", res.Severity, res.Message)
	}
}

func TestCheckPortRangeSmall(t *testing.T) {
	cfg := &config.ProcessConfig{PortRangeStart: 10000, PortRangeEnd: 10004} // 5 ports
	res := checkPortRange(cfg)
	if res.Severity != preflight.SeverityWarning {
		t.Errorf("severity = %v, want Warning", res.Severity)
	}
}

func TestCheckPortRangeAdequate(t *testing.T) {
	cfg := &config.ProcessConfig{PortRangeStart: 10000, PortRangeEnd: 10999} // 1000 ports
	res := checkPortRange(cfg)
	if res.Severity != preflight.SeverityOK {
		t.Errorf("severity = %v, want OK", res.Severity)
	}
}

func TestCheckResourceLimitsNoneSet(t *testing.T) {
	res := checkResourceLimits(&config.ServerConfig{})
	if res.Severity != preflight.SeverityOK {
		t.Errorf("severity = %v, want OK", res.Severity)
	}
}

func TestCheckResourceLimitsMemorySet(t *testing.T) {
	res := checkResourceLimits(&config.ServerConfig{DefaultMemoryLimit: "2g"})
	if res.Severity != preflight.SeverityWarning {
		t.Errorf("severity = %v, want Warning", res.Severity)
	}
	if !strings.Contains(res.Message, "default_memory_limit") {
		t.Errorf("message missing memory reference: %q", res.Message)
	}
}

func TestCheckResourceLimitsCPUSet(t *testing.T) {
	res := checkResourceLimits(&config.ServerConfig{DefaultCPULimit: 4.0})
	if res.Severity != preflight.SeverityWarning {
		t.Errorf("severity = %v, want Warning", res.Severity)
	}
	if !strings.Contains(res.Message, "default_cpu_limit") {
		t.Errorf("message missing cpu reference: %q", res.Message)
	}
}

func TestCheckResourceLimitsBothSet(t *testing.T) {
	res := checkResourceLimits(&config.ServerConfig{
		DefaultMemoryLimit: "2g",
		DefaultCPULimit:    4.0,
	})
	if res.Severity != preflight.SeverityWarning {
		t.Errorf("severity = %v, want Warning", res.Severity)
	}
	if !strings.Contains(res.Message, "default_memory_limit") || !strings.Contains(res.Message, "default_cpu_limit") {
		t.Errorf("message missing a limit reference: %q", res.Message)
	}
}

func TestParseStatusUID(t *testing.T) {
	cases := []struct {
		in    string
		want  int
		isErr bool
	}{
		{"Uid:\t1000\t1000\t1000\t1000", 1000, false},
		{"Gid:\t65534\t65534\t65534\t65534", 65534, false},
		{"Uid:\t0", 0, false},
		{"Uid:", 0, true},
		{"", 0, true},
		{"Uid:\tnope", 0, true},
	}
	for _, tc := range cases {
		got, err := parseStatusUID(tc.in)
		if tc.isErr {
			if err == nil {
				t.Errorf("parseStatusUID(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseStatusUID(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseStatusUID(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestCheckUserNamespacesReturnsValidResult(t *testing.T) {
	// The sysctl is either present (enabled or disabled) or absent
	// (default allow). All three cases are valid outcomes — this test
	// just verifies the function returns a well-formed result for
	// whatever the current host looks like.
	res := checkUserNamespaces()
	if res.Name != "user_namespaces" {
		t.Errorf("name = %q, want user_namespaces", res.Name)
	}
	if res.Category != "process" {
		t.Errorf("category = %q, want process", res.Category)
	}
	// When the file exists and is "0", expect Error; otherwise OK.
	data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	wantErr := err == nil && strings.TrimSpace(string(data)) == "0"
	switch res.Severity {
	case preflight.SeverityError:
		if !wantErr {
			t.Errorf("unexpected Error: %q", res.Message)
		}
	case preflight.SeverityOK:
		if wantErr {
			t.Errorf("expected Error (sysctl=0), got OK")
		}
	default:
		t.Errorf("unexpected severity %v", res.Severity)
	}
}

func TestRunPreflightPopulatesReport(t *testing.T) {
	// RunPreflight dispatches all the individual checks and aggregates
	// their results. We don't care about pass/fail here — the system may
	// or may not have a working bwrap — we care that the report contains
	// all the expected check names.
	cfg := &config.ProcessConfig{
		BwrapPath:      "/nonexistent/bwrap",
		RPath:          "/nonexistent/R",
		PortRangeStart: 10000,
		PortRangeEnd:   10999,
		WorkerUIDStart: 60000,
		WorkerUIDEnd:   60999,
		WorkerGID:      65534,
	}
	fullCfg := &config.Config{Process: cfg}
	report := RunPreflight(cfg, fullCfg)
	if report == nil {
		t.Fatal("expected non-nil report")
	}
	expected := map[string]bool{
		"bwrap_available":        false,
		"r_binary":               false,
		"user_namespaces":        false,
		"port_range":             false,
		"resource_limits":        false,
		"bwrap_host_uid_mapping": false,
		"worker_egress":          false,
	}
	for _, r := range report.Results {
		if _, ok := expected[r.Name]; ok {
			expected[r.Name] = true
		}
	}
	for name, present := range expected {
		if !present {
			t.Errorf("report missing check %q", name)
		}
	}
}
