package process

import (
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
