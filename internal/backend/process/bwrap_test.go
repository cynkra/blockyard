package process

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
)

// indexOf returns the index of the first occurrence of needle in
// haystack, or -1.
func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}

func assertContains(t *testing.T, args []string, want string) {
	t.Helper()
	if indexOf(args, want) < 0 {
		t.Errorf("expected %q in args, got %v", want, args)
	}
}

func assertFlagValue(t *testing.T, args []string, flag, want string) {
	t.Helper()
	idx := indexOf(args, flag)
	if idx < 0 {
		t.Errorf("flag %q not found in args", flag)
		return
	}
	if idx+1 >= len(args) {
		t.Errorf("flag %q has no value", flag)
		return
	}
	if args[idx+1] != want {
		t.Errorf("flag %q = %q, want %q", flag, args[idx+1], want)
	}
}

// assertBindMount verifies that args contains a sequence
// `kind src dst` (e.g. "--ro-bind /a /b").
func assertBindMount(t *testing.T, args []string, kind, src, dst string) {
	t.Helper()
	for i := 0; i+2 < len(args); i++ {
		if args[i] == kind && args[i+1] == src && args[i+2] == dst {
			return
		}
	}
	t.Errorf("expected %s %s %s in args, got %v", kind, src, dst, args)
}

func TestBwrapArgs(t *testing.T) {
	cfg := &config.ProcessConfig{
		BwrapPath: "/usr/bin/bwrap",
		RPath:     "/usr/bin/R",
	}
	spec := backend.WorkerSpec{
		WorkerID:    "w-1",
		BundlePath:  "/data/bundles/app1/v1",
		WorkerMount: "/app",
		ShinyPort:   3838,
	}

	args := bwrapArgs(cfg, spec, 10000, 60000, 65534)

	assertContains(t, args, "--unshare-pid")
	assertContains(t, args, "--unshare-user")
	assertContains(t, args, "--die-with-parent")
	assertFlagValue(t, args, "--uid", "60000")
	assertFlagValue(t, args, "--gid", "65534")
	assertBindMount(t, args, "--ro-bind", "/", "/")
	assertBindMount(t, args, "--ro-bind", spec.BundlePath, spec.WorkerMount)

	// Verify the R command is after the -- separator.
	sepIdx := indexOf(args, "--")
	if sepIdx < 0 {
		t.Fatal("missing -- separator")
	}
	if args[sepIdx+1] != cfg.RPath {
		t.Errorf("expected R path %q after --, got %q", cfg.RPath, args[sepIdx+1])
	}

	// Default Cmd uses runApp with the allocated port.
	rest := strings.Join(args[sepIdx+1:], " ")
	if !strings.Contains(rest, "port=10000") {
		t.Errorf("default cmd should reference port=10000: %s", rest)
	}
}

func TestBwrapArgsWithLibDir(t *testing.T) {
	cfg := &config.ProcessConfig{
		BwrapPath: "/usr/bin/bwrap",
		RPath:     "/usr/bin/R",
	}
	spec := backend.WorkerSpec{
		WorkerID:    "w-1",
		BundlePath:  "/data/bundles/app1/v1",
		LibDir:      "/data/.pkg-store/abc123",
		WorkerMount: "/app",
	}

	args := bwrapArgs(cfg, spec, 10001, 60001, 65534)
	assertBindMount(t, args, "--ro-bind", spec.LibDir, "/blockyard-lib-store")
}

func TestBwrapArgsLegacyLibrary(t *testing.T) {
	cfg := &config.ProcessConfig{
		BwrapPath: "/usr/bin/bwrap",
		RPath:     "/usr/bin/R",
	}
	spec := backend.WorkerSpec{
		WorkerID:    "w-1",
		BundlePath:  "/data/bundles/app1/v1",
		LibraryPath: "/data/legacy-lib",
		WorkerMount: "/app",
	}

	args := bwrapArgs(cfg, spec, 10002, 60002, 65534)
	assertBindMount(t, args, "--ro-bind", spec.LibraryPath, "/blockyard-lib")
}

func TestBwrapArgsCustomCmd(t *testing.T) {
	cfg := &config.ProcessConfig{
		BwrapPath: "/usr/bin/bwrap",
		RPath:     "/usr/bin/R",
	}
	spec := backend.WorkerSpec{
		WorkerID:    "w-1",
		BundlePath:  "/data/bundles/app1/v1",
		WorkerMount: "/app",
		Cmd:         []string{"/usr/bin/R", "-e", "httpuv::runServer('0.0.0.0', 8080)"},
	}

	args := bwrapArgs(cfg, spec, 10002, 60002, 65534)
	sepIdx := indexOf(args, "--")
	cmd := args[sepIdx+1:]
	if len(cmd) != 3 || cmd[0] != "/usr/bin/R" {
		t.Errorf("expected custom command after --, got %v", cmd)
	}
}

func TestBwrapArgsTokenAndTransfer(t *testing.T) {
	cfg := &config.ProcessConfig{
		BwrapPath: "/usr/bin/bwrap",
		RPath:     "/usr/bin/R",
	}
	spec := backend.WorkerSpec{
		WorkerID:    "w-1",
		BundlePath:  "/data/bundles/app1/v1",
		WorkerMount: "/app",
		TokenDir:    "/data/.worker-tokens/w-1",
		TransferDir: "/data/.transfers/w-1",
	}

	args := bwrapArgs(cfg, spec, 10003, 60003, 65534)
	assertBindMount(t, args, "--ro-bind", spec.TokenDir, "/var/run/blockyard")
	assertBindMount(t, args, "--bind", spec.TransferDir, "/transfer")
}

func TestBwrapBuildArgs(t *testing.T) {
	cfg := &config.ProcessConfig{
		BwrapPath: "/usr/bin/bwrap",
		RPath:     "/usr/bin/R",
	}
	spec := backend.BuildSpec{
		Cmd: []string{"/usr/bin/R", "-e", "pak::pak_install()"},
		Mounts: []backend.MountEntry{
			{Source: "/data/worker-lib", Target: "/worker-lib", ReadOnly: true},
			{Source: "/data/.pkg-store", Target: "/store", ReadOnly: false},
		},
	}

	args := bwrapBuildArgs(cfg, spec, 60000, 65534)
	assertBindMount(t, args, "--ro-bind", "/data/worker-lib", "/worker-lib")
	assertBindMount(t, args, "--bind", "/data/.pkg-store", "/store")
	assertFlagValue(t, args, "--uid", "60000")
	assertFlagValue(t, args, "--gid", "65534")
}

func TestApplySeccompEmpty(t *testing.T) {
	cmd := exec.Command("/bin/true")
	args, cleanup, err := applySeccomp(cmd, "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if args != nil {
		t.Errorf("expected nil args for empty profile, got %v", args)
	}
	if cleanup == nil {
		t.Error("cleanup must not be nil; callers defer it unconditionally")
	} else {
		cleanup() // no-op should not panic
	}
}

func TestApplySeccompMissingFile(t *testing.T) {
	cmd := exec.Command("/bin/true")
	_, cleanup, err := applySeccomp(cmd, "/nonexistent/seccomp.bpf")
	if err == nil {
		t.Fatal("expected error for missing profile")
	}
	if cleanup == nil {
		t.Error("cleanup must not be nil even on error")
	} else {
		cleanup() // no-op should not panic
	}
}

func TestApplySeccompRealFile(t *testing.T) {
	// Create a small temp file that stands in for a compiled BPF
	// profile. applySeccomp only opens it and hands the fd to bwrap;
	// we just verify the fd wiring, not the content.
	tmp, err := os.CreateTemp("", "seccomp-*.bpf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString("fake")
	tmp.Close()

	cmd := exec.Command("/bin/true")
	args, cleanup, err := applySeccomp(cmd, tmp.Name())
	if err != nil {
		t.Fatalf("applySeccomp: %v", err)
	}
	defer cleanup()
	if len(args) != 2 || args[0] != "--seccomp" {
		t.Errorf("unexpected args: %v", args)
	}
	if args[1] != "3" {
		t.Errorf("expected fd 3 for the first extra file, got %q", args[1])
	}
	if len(cmd.ExtraFiles) != 1 {
		t.Errorf("expected one ExtraFile, got %d", len(cmd.ExtraFiles))
	}
}

func TestSpliceBeforeSeparator(t *testing.T) {
	cmd := exec.Command("bwrap", "--ro-bind", "/", "/", "--", "/bin/sh")
	spliceBeforeSeparator(cmd, []string{"--seccomp", "3"})
	want := []string{"bwrap", "--ro-bind", "/", "/", "--seccomp", "3", "--", "/bin/sh"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("wrong length: got %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, cmd.Args[i], want[i])
		}
	}
}

func TestSpliceBeforeSeparatorMissingSeparator(t *testing.T) {
	// When the separator is absent (shouldn't happen with well-formed
	// bwrap args), the helper falls back to appending.
	cmd := exec.Command("bwrap", "--ro-bind", "/", "/")
	spliceBeforeSeparator(cmd, []string{"--seccomp", "3"})
	if cmd.Args[len(cmd.Args)-1] != "3" {
		t.Errorf("expected appended args, got %v", cmd.Args)
	}
}
