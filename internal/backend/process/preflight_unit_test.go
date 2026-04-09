package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/preflight"
)

// TestCheckBwrap covers all three branches of checkBwrap. /bin/echo
// stands in for a working bwrap: it's on PATH and prints something
// for --version; /bin/false is present but exits non-zero. The real
// bwrap may not run in this environment.
func TestCheckBwrap(t *testing.T) {
	cases := []struct {
		name         string
		bwrapPath    string
		wantSeverity preflight.Severity
		wantContains string
	}{
		{
			name:         "missing",
			bwrapPath:    "/nonexistent/bwrap",
			wantSeverity: preflight.SeverityError,
			wantContains: "bwrap not found",
		},
		{
			name:         "version_succeeds",
			bwrapPath:    "/bin/echo",
			wantSeverity: preflight.SeverityOK,
			wantContains: "bwrap version",
		},
		{
			name:         "version_fails",
			bwrapPath:    "/bin/false",
			wantSeverity: preflight.SeverityError,
			wantContains: "--version",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := checkBwrap(&config.ProcessConfig{BwrapPath: tc.bwrapPath})
			if res.Name != "bwrap_available" {
				t.Errorf("name = %q, want bwrap_available", res.Name)
			}
			if res.Category != "process" {
				t.Errorf("category = %q, want process", res.Category)
			}
			if res.Severity != tc.wantSeverity {
				t.Errorf("severity = %v, want %v; message: %q",
					res.Severity, tc.wantSeverity, res.Message)
			}
			if !strings.Contains(res.Message, tc.wantContains) {
				t.Errorf("message should contain %q: %q", tc.wantContains, res.Message)
			}
		})
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

// TestCheckUserNamespacesAt covers all three sysctl branches against
// fixture files, deterministically.
func TestCheckUserNamespacesAt(t *testing.T) {
	dir := t.TempDir()

	// Absent file → OK, "default allow" message.
	absent := filepath.Join(dir, "missing")
	res := checkUserNamespacesAt(absent)
	if res.Severity != preflight.SeverityOK {
		t.Errorf("absent: severity = %v, want OK", res.Severity)
	}
	if !strings.Contains(res.Message, "default allow") {
		t.Errorf("absent: message should mention default allow: %q", res.Message)
	}

	// Present and = "0" → Error, actionable message.
	disabled := filepath.Join(dir, "disabled")
	if err := os.WriteFile(disabled, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = checkUserNamespacesAt(disabled)
	if res.Severity != preflight.SeverityError {
		t.Errorf("disabled: severity = %v, want Error", res.Severity)
	}
	if !strings.Contains(res.Message, "unprivileged_userns_clone") {
		t.Errorf("disabled: message should name the sysctl: %q", res.Message)
	}

	// Present and = "1" → OK, "enabled" message.
	enabled := filepath.Join(dir, "enabled")
	if err := os.WriteFile(enabled, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = checkUserNamespacesAt(enabled)
	if res.Severity != preflight.SeverityOK {
		t.Errorf("enabled: severity = %v, want OK", res.Severity)
	}
	if !strings.Contains(res.Message, "enabled") {
		t.Errorf("enabled: message should say enabled: %q", res.Message)
	}
}

// TestCheckWorkerEgressAggregation covers the three severity
// branches by swapping probeReachableFn for a deterministic fake.
func TestCheckWorkerEgressAggregation(t *testing.T) {
	cfg := &config.ProcessConfig{
		WorkerUIDStart: 60000,
		WorkerUIDEnd:   60099,
		WorkerGID:      65534,
	}
	// Include redis + openbao + postgres so we have multiple non-
	// critical targets alongside the always-critical cloud metadata.
	fullCfg := &config.Config{
		Process: cfg,
		Redis:   &config.RedisConfig{URL: "redis://redis.internal:6379"},
		Openbao: &config.OpenbaoConfig{Address: "https://openbao.internal:8200"},
		Database: config.DatabaseConfig{
			Driver: "postgres",
			URL:    "postgres://u:p@db.internal:5432/app",
		},
	}

	restore := probeReachableFn
	t.Cleanup(func() { probeReachableFn = restore })

	cases := []struct {
		name         string
		reachable    map[string]bool // target addr → reachable?
		wantSeverity preflight.Severity
		wantContains string
	}{
		{
			name: "all_blocked",
			reachable: map[string]bool{
				"169.254.169.254:80":  false,
				"redis.internal:6379": false,
				"openbao.internal:8200": false,
				"db.internal:5432":    false,
			},
			wantSeverity: preflight.SeverityOK,
			wantContains: "blocked",
		},
		{
			name: "metadata_reachable_is_error",
			reachable: map[string]bool{
				"169.254.169.254:80":  true,
				"redis.internal:6379": false,
				"openbao.internal:8200": false,
				"db.internal:5432":    false,
			},
			wantSeverity: preflight.SeverityError,
			wantContains: "cloud_metadata",
		},
		{
			name: "non_critical_reachable_is_warning",
			reachable: map[string]bool{
				"169.254.169.254:80":  false,
				"redis.internal:6379": true,
				"openbao.internal:8200": false,
				"db.internal:5432":    false,
			},
			wantSeverity: preflight.SeverityWarning,
			wantContains: "redis",
		},
		{
			name: "metadata_plus_non_critical_escalates_to_error",
			reachable: map[string]bool{
				"169.254.169.254:80":  true,
				"redis.internal:6379": true,
				"openbao.internal:8200": false,
				"db.internal:5432":    false,
			},
			wantSeverity: preflight.SeverityError,
			wantContains: "169.254",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probeReachableFn = func(_ *config.ProcessConfig, _ int, _ int, target string) bool {
				return tc.reachable[target]
			}
			res := checkWorkerEgress(cfg, fullCfg)
			if res.Severity != tc.wantSeverity {
				t.Errorf("severity = %v, want %v; message: %q",
					res.Severity, tc.wantSeverity, res.Message)
			}
			if !strings.Contains(res.Message, tc.wantContains) {
				t.Errorf("message should contain %q: %q", tc.wantContains, res.Message)
			}
		})
	}
}

// TestCheckWorkerEgressNoOptionalTargets — cloud_metadata is always
// probed even without any Redis/OpenBao/database configuration.
func TestCheckWorkerEgressNoOptionalTargets(t *testing.T) {
	cfg := &config.ProcessConfig{WorkerUIDStart: 60000, WorkerGID: 65534}
	fullCfg := &config.Config{Process: cfg}
	restore := probeReachableFn
	t.Cleanup(func() { probeReachableFn = restore })

	var targets []string
	probeReachableFn = func(_ *config.ProcessConfig, _ int, _ int, target string) bool {
		targets = append(targets, target)
		return false // metadata blocked → OK
	}
	res := checkWorkerEgress(cfg, fullCfg)
	if res.Severity != preflight.SeverityOK {
		t.Errorf("severity = %v, want OK", res.Severity)
	}
	if len(targets) != 1 || targets[0] != "169.254.169.254:80" {
		t.Errorf("expected only metadata probe, got %v", targets)
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
		return // unreachable; satisfies staticcheck SA5011
	}
	expected := map[string]bool{
		"bwrap_available":        false,
		"r_binary":               false,
		"user_namespaces":        false,
		"port_range":             false,
		"resource_limits":        false,
		"seccomp_profile":        false,
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

// TestCheckSeccompProfile covers every branch of checkSeccompProfile.
// The check is new in phase 3-8 — it catches the "operator set the
// path but the file is missing or unreadable" footgun at startup
// instead of at first worker spawn. Each case maps to a distinct
// failure mode the check must surface.
func TestCheckSeccompProfile(t *testing.T) {
	dir := t.TempDir()

	// Valid: a regular file with non-zero content. The check does not
	// validate BPF content — libseccomp handles that at bwrap time.
	validPath := filepath.Join(dir, "valid.bpf")
	if err := os.WriteFile(validPath, []byte("not-real-bpf-but-non-empty"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Empty file — must be flagged; operators sometimes `touch` the
	// path and move on without running seccomp-compile.
	emptyPath := filepath.Join(dir, "empty.bpf")
	if err := os.WriteFile(emptyPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Not a regular file — use the temp dir itself, which is a
	// directory. Stat succeeds but Mode().IsRegular() is false.
	notRegularPath := dir

	cases := []struct {
		name         string
		path         string
		wantSeverity preflight.Severity
		wantContains string
	}{
		{
			name:         "empty_path_is_ok",
			path:         "",
			wantSeverity: preflight.SeverityOK,
			wantContains: "no seccomp profile",
		},
		{
			name:         "missing_file",
			path:         filepath.Join(dir, "nope.bpf"),
			wantSeverity: preflight.SeverityError,
			wantContains: "install-seccomp",
		},
		{
			name:         "not_regular_file",
			path:         notRegularPath,
			wantSeverity: preflight.SeverityError,
			wantContains: "not a regular file",
		},
		{
			name:         "empty_file",
			path:         emptyPath,
			wantSeverity: preflight.SeverityError,
			wantContains: "is empty",
		},
		{
			name:         "valid_file",
			path:         validPath,
			wantSeverity: preflight.SeverityOK,
			wantContains: "readable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := checkSeccompProfile(&config.ProcessConfig{SeccompProfile: tc.path})
			if res.Name != "seccomp_profile" {
				t.Errorf("name = %q, want seccomp_profile", res.Name)
			}
			if res.Category != "process" {
				t.Errorf("category = %q, want process", res.Category)
			}
			if res.Severity != tc.wantSeverity {
				t.Errorf("severity = %v, want %v; message: %q",
					res.Severity, tc.wantSeverity, res.Message)
			}
			if !strings.Contains(res.Message, tc.wantContains) {
				t.Errorf("message should contain %q: %q", tc.wantContains, res.Message)
			}
		})
	}
}
