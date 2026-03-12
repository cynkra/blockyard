//go:build docker_test

package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/rvcache"
)

func testConfig() *config.DockerConfig {
	return &config.DockerConfig{
		Socket:    "/var/run/docker.sock",
		Image:     "alpine:latest",
		ShinyPort: 8080,
		RvVersion: "v0.19.0",
	}
}

// testRvBinary creates a dummy rv executable for build tests.
func testRvBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "rv")
	// Minimal shell script that acts as a no-op rv sync.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write dummy rv: %v", err)
	}
	return bin
}

func TestSpawnAndStop(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       "alpine:latest",
		Cmd:         []string{"sleep", "300"},
		BundlePath:  "/tmp",
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		Labels:      map[string]string{},
	}

	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Container should have an address
	addr, err := b.Addr(ctx, workerID)
	if err != nil {
		t.Fatalf("Addr: %v", err)
	}
	if addr == "" {
		t.Fatal("Addr returned empty string")
	}
	t.Logf("worker addr: %s", addr)

	// Stop and clean up
	if err := b.Stop(ctx, workerID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Addr should fail after stop
	if _, err := b.Addr(ctx, workerID); err == nil {
		t.Fatal("Addr should fail after Stop")
	}
}

func TestHealthCheckNoListener(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       "alpine:latest",
		Cmd:         []string{"sleep", "300"},
		BundlePath:  "/tmp",
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		Labels:      map[string]string{},
	}

	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer b.Stop(ctx, workerID)

	time.Sleep(500 * time.Millisecond)

	if b.HealthCheck(ctx, workerID) {
		t.Fatal("HealthCheck should return false when nothing is listening")
	}
}

func TestOrphanCleanup(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       "alpine:latest",
		Cmd:         []string{"sleep", "300"},
		BundlePath:  "/tmp",
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		Labels:      map[string]string{},
	}

	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Simulate crash — don't call Stop, just list and clean up
	managed, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(managed) == 0 {
		t.Fatal("expected managed resources after spawn")
	}

	for _, r := range managed {
		if err := b.RemoveResource(ctx, r); err != nil {
			t.Logf("RemoveResource warning: %v", err)
		}
	}

	// Should be clean now
	remaining, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged after cleanup: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected 0 remaining resources, got %d", len(remaining))
	}
}

func TestNetworkIsolation(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id1 := "test-" + uuid.New().String()[:8]
	id2 := "test-" + uuid.New().String()[:8]
	makeSpec := func(id string) backend.WorkerSpec {
		return backend.WorkerSpec{
			AppID:       "test-app",
			WorkerID:    id,
			Image:       "alpine:latest",
			Cmd:         []string{"sleep", "300"},
			BundlePath:  "/tmp",
			LibraryPath: "",
			WorkerMount: "/app",
			ShinyPort:   8080,
			Labels:      map[string]string{},
		}
	}

	if err := b.Spawn(ctx, makeSpec(id1)); err != nil {
		t.Fatalf("Spawn worker 1: %v", err)
	}
	defer b.Stop(ctx, id1)

	if err := b.Spawn(ctx, makeSpec(id2)); err != nil {
		t.Fatalf("Spawn worker 2: %v", err)
	}
	defer b.Stop(ctx, id2)

	addr1, err := b.Addr(ctx, id1)
	if err != nil {
		t.Fatalf("Addr worker 1: %v", err)
	}
	addr2, err := b.Addr(ctx, id2)
	if err != nil {
		t.Fatalf("Addr worker 2: %v", err)
	}

	if addr1 == addr2 {
		t.Fatalf("workers should have different addresses, both got %s", addr1)
	}

	// Verify they cannot reach each other
	b.mu.Lock()
	ws1 := b.workers[id1]
	b.mu.Unlock()

	ip2 := strings.Split(addr2, ":")[0]
	execResp, err := b.client.ContainerExecCreate(ctx, ws1.containerID,
		container.ExecOptions{
			Cmd: []string{"sh", "-c", fmt.Sprintf(
				"wget -q -O /dev/null --timeout=2 http://%s:%d/ 2>&1 || exit 1",
				ip2, 8080,
			)},
		},
	)
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}

	if err := b.client.ContainerExecStart(ctx, execResp.ID, container.ExecStartOptions{}); err != nil {
		t.Fatalf("ExecStart: %v", err)
	}

	time.Sleep(3 * time.Second)
	inspect, err := b.client.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		t.Fatalf("ExecInspect: %v", err)
	}

	if inspect.ExitCode == 0 {
		t.Fatal("worker 1 should NOT be able to reach worker 2 (network isolation broken)")
	}
}

func TestMetadataEndpointBlocked(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       "alpine:latest",
		Cmd:         []string{"sleep", "300"},
		BundlePath:  "/tmp",
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		Labels:      map[string]string{},
	}

	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer b.Stop(ctx, workerID)

	time.Sleep(500 * time.Millisecond)

	b.mu.Lock()
	ws := b.workers[workerID]
	b.mu.Unlock()

	execResp, err := b.client.ContainerExecCreate(ctx, ws.containerID,
		container.ExecOptions{
			Cmd: []string{"wget", "--spider", "--timeout=2",
				"http://169.254.169.254/"},
		},
	)
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}

	if err := b.client.ContainerExecStart(ctx, execResp.ID, container.ExecStartOptions{}); err != nil {
		t.Fatalf("ExecStart: %v", err)
	}

	time.Sleep(3 * time.Second)
	inspect, err := b.client.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		t.Fatalf("ExecInspect: %v", err)
	}

	if inspect.ExitCode == 0 {
		t.Fatal("metadata endpoint should be blocked but request succeeded")
	}
}

func testSpawn(t *testing.T, b *DockerBackend, cmd []string) (string, backend.WorkerSpec) {
	t.Helper()
	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       "alpine:latest",
		Cmd:         cmd,
		BundlePath:  "/tmp",
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		Labels:      map[string]string{},
	}
	if err := b.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { b.Stop(context.Background(), workerID) })
	return workerID, spec
}

func TestLogs(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID, _ := testSpawn(t, b, []string{"sh", "-c", "echo hello; echo world; sleep 300"})
	time.Sleep(1 * time.Second)

	stream, err := b.Logs(ctx, workerID)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer stream.Close()

	var lines []string
	timeout := time.After(5 * time.Second)
	for len(lines) < 2 {
		select {
		case line, ok := <-stream.Lines:
			if !ok {
				t.Fatalf("log stream closed early, got %d lines: %v", len(lines), lines)
			}
			lines = append(lines, line)
		case <-timeout:
			t.Fatalf("timed out waiting for log lines, got %d: %v", len(lines), lines)
		}
	}

	if lines[0] != "hello" || lines[1] != "world" {
		t.Errorf("unexpected log lines: %v", lines)
	}
}

func TestLogsUnknownWorker(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = b.Logs(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown worker")
	}
}

func TestStopUnknownWorker(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := b.Stop(ctx, "nonexistent"); err == nil {
		t.Fatal("expected error for unknown worker")
	}
}

func TestHealthCheckUnknownWorker(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if b.HealthCheck(ctx, "nonexistent") {
		t.Fatal("expected false for unknown worker")
	}
}

func testBundleDir(t *testing.T) (bundleDir, libDir string) {
	t.Helper()
	bundleDir = t.TempDir()
	libDir = filepath.Join(bundleDir, "rv", "library")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return bundleDir, libDir
}

func TestBuildFailsWithBadImage(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bundleDir, libDir := testBundleDir(t)
	spec := backend.BuildSpec{
		AppID:        "test-app",
		BundleID:     uuid.New().String()[:8],
		Image:        "alpine:latest",
		RvBinaryPath: "/nonexistent/rv",
		BundlePath:   bundleDir,
		LibraryPath:  libDir,
		Labels:       map[string]string{},
	}

	// rv binary doesn't exist — container creation should fail
	_, err = b.Build(ctx, spec)
	if err == nil {
		t.Error("expected build to fail with nonexistent rv binary")
	}
}

func TestBuildWithProductionImage(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bundleDir, libDir := testBundleDir(t)
	if err := os.WriteFile(filepath.Join(bundleDir, "app.R"), []byte("# empty\n"), 0o644); err != nil {
		t.Fatalf("write app.R: %v", err)
	}

	rvBin := testRvBinary(t)
	spec := backend.BuildSpec{
		AppID:        "test-app",
		BundleID:     uuid.New().String()[:8],
		Image:        "ghcr.io/rocker-org/r-ver:latest",
		RvBinaryPath: rvBin,
		BundlePath:   bundleDir,
		LibraryPath:  libDir,
		Labels:       map[string]string{},
	}

	result, err := b.Build(ctx, spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !result.Success {
		t.Errorf("expected build to succeed with mounted rv binary, got exit code %d", result.ExitCode)
	}
}

func TestAddrUnknownWorker(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = b.Addr(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown worker")
	}
}

// TestBuildE2E_RvSync exercises the full build pipeline: real rv binary,
// real rocker image, real rproject.toml + rv.lock. This is the test that
// catches asset-format changes in rv releases and Docker mount issues.
func TestBuildE2E_RvSync(t *testing.T) {
	const rvVersion = "v0.19.0"
	const image = "ghcr.io/rocker-org/r-ver:4.4.3"

	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 1. Download the real rv binary via rvcache.
	cacheDir := filepath.Join(t.TempDir(), "rv-cache")
	rvBin, err := rvcache.EnsureBinary(ctx, cacheDir, rvVersion)
	if err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}

	// 2. Create a minimal bundle with rproject.toml, rv.lock, and app.R.
	bundleDir := t.TempDir()
	libDir := t.TempDir()

	rprojectToml := `[project]
name = "e2e-test"
r_version = "4.4"
repositories = [
    {alias = "CRAN", url = "https://cloud.r-project.org/"},
]
dependencies = [
    "mime",
]
`
	rvLock := `# This file is automatically @generated by rv.
# It is not intended for manual editing.
version = 2
r_version = "4.4"

[[packages]]
name = "mime"
version = "0.13"
source = { repository = "https://cloud.r-project.org/" }
force_source = false
dependencies = []
`
	for name, content := range map[string]string{
		"rproject.toml": rprojectToml,
		"rv.lock":       rvLock,
		"app.R":         "library(mime)\n",
	} {
		if err := os.WriteFile(filepath.Join(bundleDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// 3. Run the build.
	spec := backend.BuildSpec{
		AppID:        "test-app",
		BundleID:     uuid.New().String()[:8],
		Image:        image,
		RvBinaryPath: rvBin,
		BundlePath:   bundleDir,
		LibraryPath:  libDir,
		Labels:       map[string]string{},
	}

	result, err := b.Build(ctx, spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !result.Success {
		t.Fatalf("build failed with exit code %d", result.ExitCode)
	}

	// 4. Verify that mime was installed into the library dir.
	matches, err := filepath.Glob(filepath.Join(libDir, "*", "*", "*", "mime"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		// List what's actually in libDir for debugging.
		var found []string
		filepath.Walk(libDir, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				rel, _ := filepath.Rel(libDir, path)
				found = append(found, rel)
			}
			return nil
		})
		t.Fatalf("mime package not found in library dir; contents: %v", found)
	}
}
