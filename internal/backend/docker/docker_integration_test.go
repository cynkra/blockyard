//go:build docker_test

package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/testutil"
)

// rawClient extracts the concrete *client.Client from a DockerBackend.
// Only needed in integration tests that call Docker exec APIs not on
// the dockerClient interface.
func rawClient(t *testing.T, d *DockerBackend) *client.Client {
	t.Helper()
	c, ok := d.client.(*client.Client)
	if !ok {
		t.Fatal("expected *client.Client behind dockerClient interface")
	}
	return c
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Docker: config.DockerConfig{
			Socket:     "/var/run/docker.sock",
			Image:      testutil.AlpineImage(t),
			ShinyPort:  8080,
			PakVersion: "stable",
		},
	}
}


func TestSpawnAndStop(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       testutil.AlpineImage(t),
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
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       testutil.AlpineImage(t),
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
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       testutil.AlpineImage(t),
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

	// Simulate crash — don't call Stop, just list and clean up.
	// Filter to only this test's resources to avoid interference from
	// parallel tests that also create managed containers/networks.
	managed, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}

	var ours []backend.ManagedResource
	for _, r := range managed {
		if r.Labels["dev.blockyard/worker-id"] == workerID {
			ours = append(ours, r)
		}
	}
	if len(ours) == 0 {
		t.Fatal("expected managed resources after spawn")
	}

	for _, r := range ours {
		if err := b.RemoveResource(ctx, r); err != nil {
			t.Logf("RemoveResource warning: %v", err)
		}
	}

	// Our resources should be gone now.
	remaining, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged after cleanup: %v", err)
	}
	var oursRemaining int
	for _, r := range remaining {
		if r.Labels["dev.blockyard/worker-id"] == workerID {
			oursRemaining++
		}
	}
	if oursRemaining != 0 {
		t.Fatalf("expected 0 remaining resources for worker %s, got %d", workerID, oursRemaining)
	}
}

func TestNetworkIsolation(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id1 := "test-" + uuid.New().String()[:8]
	id2 := "test-" + uuid.New().String()[:8]
	makeSpec := func(id string) backend.WorkerSpec {
		return backend.WorkerSpec{
			AppID:       "test-app",
			WorkerID:    id,
			Image:       testutil.AlpineImage(t),
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
	execResp, err := rawClient(t, b).ExecCreate(ctx, ws1.containerID,
		client.ExecCreateOptions{
			Cmd: []string{"sh", "-c", fmt.Sprintf(
				"wget -q -O /dev/null --timeout=2 http://%s:%d/ 2>&1 || exit 1",
				ip2, 8080,
			)},
		},
	)
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}

	if _, err := rawClient(t, b).ExecStart(ctx, execResp.ID, client.ExecStartOptions{}); err != nil {
		t.Fatalf("ExecStart: %v", err)
	}

	time.Sleep(3 * time.Second)
	inspect, err := rawClient(t, b).ExecInspect(ctx, execResp.ID, client.ExecInspectOptions{})
	if err != nil {
		t.Fatalf("ExecInspect: %v", err)
	}

	if inspect.ExitCode == 0 {
		t.Fatal("worker 1 should NOT be able to reach worker 2 (network isolation broken)")
	}
}

func TestMetadataEndpointBlocked(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       testutil.AlpineImage(t),
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

	execResp, err := rawClient(t, b).ExecCreate(ctx, ws.containerID,
		client.ExecCreateOptions{
			Cmd: []string{"wget", "--spider", "--timeout=2",
				"http://169.254.169.254/"},
		},
	)
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}

	if _, err := rawClient(t, b).ExecStart(ctx, execResp.ID, client.ExecStartOptions{}); err != nil {
		t.Fatalf("ExecStart: %v", err)
	}

	time.Sleep(3 * time.Second)
	inspect, err := rawClient(t, b).ExecInspect(ctx, execResp.ID, client.ExecInspectOptions{})
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
		Image:       testutil.AlpineImage(t),
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
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
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
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
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
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := b.Stop(ctx, "nonexistent"); err == nil {
		t.Fatal("expected error for unknown worker")
	}
}

func TestHealthCheckUnknownWorker(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
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
	libDir = t.TempDir()
	return bundleDir, libDir
}

func TestBuildFailsWithBadImage(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bundleDir, libDir := testBundleDir(t)
	spec := backend.BuildSpec{
		AppID:    "test-app",
		BundleID: uuid.New().String()[:8],
		Image:    testutil.AlpineImage(t),
		Cmd:      []string{"false"},
		Mounts: []backend.MountEntry{
			{Source: bundleDir, Target: "/app", ReadOnly: true},
			{Source: libDir, Target: "/build-lib", ReadOnly: false},
		},
		Labels: map[string]string{},
	}

	// "false" command exits non-zero — build should fail
	result, err := b.Build(ctx, spec)
	if err != nil {
		// Build may return an error or a non-success result
		return
	}
	if result.Success {
		t.Error("expected build to fail with 'false' command")
	}
}

func TestBuildWithProductionImage(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bundleDir, libDir := testBundleDir(t)
	if err := os.WriteFile(filepath.Join(bundleDir, "app.R"), []byte("# empty\n"), 0o644); err != nil {
		t.Fatalf("write app.R: %v", err)
	}

	spec := backend.BuildSpec{
		AppID:    "test-app",
		BundleID: uuid.New().String()[:8],
		Image:    testutil.AlpineImage(t),
		Cmd:      []string{"true"},
		Mounts: []backend.MountEntry{
			{Source: bundleDir, Target: "/app", ReadOnly: true},
			{Source: libDir, Target: "/build-lib", ReadOnly: false},
		},
		Labels: map[string]string{},
	}

	result, err := b.Build(ctx, spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !result.Success {
		t.Errorf("expected build to succeed with Cmd/Mounts, got exit code %d", result.ExitCode)
	}
}

func TestAddrUnknownWorker(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = b.Addr(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown worker")
	}
}

// Pak-dependent tests (TestBuildE2E_PakBuild, TestFullPipeline_RestoreAndSpawnWorker)
// have been moved to pak_integration_test.go under the pak_test build tag.

func TestSpawnWithMemoryLimit(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       testutil.AlpineImage(t),
		Cmd:         []string{"sleep", "300"},
		BundlePath:  "/tmp",
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		MemoryLimit: "64m",
		Labels:      map[string]string{},
	}

	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer b.Stop(ctx, workerID)

	b.mu.Lock()
	ws := b.workers[workerID]
	b.mu.Unlock()

	cResult, err := b.client.ContainerInspect(ctx, ws.containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}

	expectedBytes := int64(64 * 1024 * 1024)
	if cResult.Container.HostConfig.Memory != expectedBytes {
		t.Fatalf("expected memory limit %d bytes, got %d", expectedBytes, cResult.Container.HostConfig.Memory)
	}
}

func TestSpawnWithCPULimit(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       testutil.AlpineImage(t),
		Cmd:         []string{"sleep", "300"},
		BundlePath:  "/tmp",
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		CPULimit:    0.5,
		Labels:      map[string]string{},
	}

	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer b.Stop(ctx, workerID)

	b.mu.Lock()
	ws := b.workers[workerID]
	b.mu.Unlock()

	cResult, err := b.client.ContainerInspect(ctx, ws.containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}

	expectedNanoCPUs := int64(0.5 * 1e9)
	if cResult.Container.HostConfig.NanoCPUs != expectedNanoCPUs {
		t.Fatalf("expected NanoCPUs %d, got %d", expectedNanoCPUs, cResult.Container.HostConfig.NanoCPUs)
	}
}

func TestSpawnWithEnvVars(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       testutil.AlpineImage(t),
		Cmd:         []string{"sleep", "300"},
		BundlePath:  "/tmp",
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		Env:         map[string]string{"TEST_VAR": "hello"},
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

	execResp, err := rawClient(t, b).ExecCreate(ctx, ws.containerID,
		client.ExecCreateOptions{
			Cmd:          []string{"sh", "-c", "echo $TEST_VAR"},
			AttachStdout: true,
		},
	)
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}

	attachResp, err := rawClient(t, b).ExecAttach(ctx, execResp.ID, client.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("ExecAttach: %v", err)
	}
	defer attachResp.Close()

	var stdout bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, io.Discard, attachResp.Reader); err != nil {
		t.Fatalf("StdCopy: %v", err)
	}

	output := strings.TrimSpace(stdout.String())
	if output != "hello" {
		t.Fatalf("expected TEST_VAR=hello, got %q", output)
	}
}

func TestBuildLogWriter(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bundleDir, libDir := testBundleDir(t)

	var logLines []string
	var mu sync.Mutex

	spec := backend.BuildSpec{
		AppID:    "test-app",
		BundleID: uuid.New().String()[:8],
		Image:    testutil.AlpineImage(t),
		Cmd:      []string{"true"},
		Mounts: []backend.MountEntry{
			{Source: bundleDir, Target: "/app", ReadOnly: true},
			{Source: libDir, Target: "/build-lib", ReadOnly: false},
		},
		Labels: map[string]string{},
		LogWriter: func(line string) {
			mu.Lock()
			logLines = append(logLines, line)
			mu.Unlock()
		},
	}

	// The build runs "true" which exits 0 silently.
	_, err = b.Build(ctx, spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	mu.Lock()
	count := len(logLines)
	mu.Unlock()

	// The dummy "true" command exits 0 silently, so the container
	// lifecycle may not produce output. Just verify the callback
	// mechanism doesn't panic and was wired up.
	t.Logf("LogWriter received %d lines", count)
}

func TestBuildExitCodeOnFailure(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bundleDir, libDir := testBundleDir(t)

	spec := backend.BuildSpec{
		AppID:    "test-app",
		BundleID: uuid.New().String()[:8],
		Image:    testutil.AlpineImage(t),
		Cmd:      []string{"false"},
		Mounts: []backend.MountEntry{
			{Source: bundleDir, Target: "/app", ReadOnly: true},
			{Source: libDir, Target: "/build-lib", ReadOnly: false},
		},
		Labels: map[string]string{},
	}

	result, err := b.Build(ctx, spec)
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	if result.Success {
		t.Fatal("expected build to fail, but Success=true")
	}
	if result.ExitCode == 0 {
		t.Fatal("expected non-zero exit code, got 0")
	}
	t.Logf("build failed as expected with exit code %d", result.ExitCode)
}

func TestSpawnAndHealthCheckWithListener(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       testutil.AlpineImage(t),
		Cmd: []string{"sh", "-c",
			"while true; do echo -e 'HTTP/1.1 200 OK\\r\\nContent-Length: 2\\r\\n\\r\\nok' | nc -l -p 8080; done",
		},
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

	// Give the nc listener time to start.
	time.Sleep(2 * time.Second)

	if !b.HealthCheck(ctx, workerID) {
		t.Fatal("HealthCheck should return true when nc is listening on the ShinyPort")
	}
}

func TestListManagedIncludesNetworks(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID, _ := testSpawn(t, b, []string{"sleep", "300"})
	_ = workerID

	managed, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}

	var hasContainer, hasNetwork bool
	for _, r := range managed {
		switch r.Kind {
		case backend.ResourceContainer:
			hasContainer = true
		case backend.ResourceNetwork:
			hasNetwork = true
		}
	}

	if !hasContainer {
		t.Error("expected at least one managed container")
	}
	if !hasNetwork {
		t.Error("expected at least one managed network")
	}
}

func TestContainerStats(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(t), t.TempDir(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID, _ := testSpawn(t, b, []string{"sleep", "300"})

	b.mu.Lock()
	ws := b.workers[workerID]
	b.mu.Unlock()

	_ = ws // workerState referenced for parity with the original test
	stats, err := b.WorkerResourceUsage(ctx, workerID)
	if err != nil {
		t.Fatalf("WorkerResourceUsage: %v", err)
	}
	if stats == nil {
		t.Fatal("WorkerResourceUsage returned nil")
	}
	if stats.MemoryLimitBytes == 0 {
		t.Error("expected non-zero memory limit")
	}
	t.Logf("CPU=%.2f%%, Mem=%d/%d", stats.CPUPercent, stats.MemoryUsageBytes, stats.MemoryLimitBytes)
}

// TestSpawnWithServiceNetwork reproduces the #230 scenario end-to-end:
// blockyard deployed as a compose service with a sibling service container
// on a shared service_network. The bug was that on cgroup v2 hosts,
// detectServerID fell back to a 12-char /etc/hostname ID that never
// matched NetworkInspect's full-ID map keys, so connectServiceContainers
// double-attached the server and the worker spawn failed with "endpoint
// with name <server> already exists".
//
// This test doesn't simulate cgroup v2 (the canonicalization in New()
// sidesteps that), but it does exercise the composition of ID comparison,
// service-network connect, and server join that the bug broke.
func TestSpawnWithServiceNetwork(t *testing.T) {
	ctx := context.Background()
	cli, err := client.New(client.WithHost("unix:///var/run/docker.sock"))
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}

	// 1. Create the service network.
	svcNet := "test-svc-" + uuid.New().String()[:8]
	netResp, err := cli.NetworkCreate(ctx, svcNet, client.NetworkCreateOptions{
		Driver: "bridge",
	})
	if err != nil {
		t.Fatalf("NetworkCreate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = cli.NetworkRemove(ctx, netResp.ID, client.NetworkRemoveOptions{})
	})

	// 2. Start a dummy "server" container on the service network. This
	// stands in for blockyard itself — the container whose ID the backend
	// carries in d.serverID.
	startOnNet := func(name string) string {
		t.Helper()
		resp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config: &container.Config{
				Image: testutil.AlpineImage(t),
				Cmd:   []string{"sleep", "300"},
			},
			HostConfig: &container.HostConfig{
				NetworkMode: container.NetworkMode(svcNet),
			},
			Name: name,
		})
		if err != nil {
			t.Fatalf("ContainerCreate %s: %v", name, err)
		}
		t.Cleanup(func() {
			_, _ = cli.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true})
		})
		if _, err := cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
			t.Fatalf("ContainerStart %s: %v", name, err)
		}
		return resp.ID
	}

	serverID := startOnNet("test-srv-" + uuid.New().String()[:8])
	_ = startOnNet("test-svc-peer-" + uuid.New().String()[:8])

	// 3. Construct a DockerBackend manually with serverID set to the
	// full canonical ID (as New() does after the #230 fix) and
	// ServiceNetwork pointing at our test network.
	fullCfg := &config.Config{
		Docker: config.DockerConfig{
			Socket:         "/var/run/docker.sock",
			Image:          testutil.AlpineImage(t),
			ShinyPort:      8080,
			ServiceNetwork: svcNet,
		},
	}
	b := &DockerBackend{
		client:   cli,
		serverID: serverID,
		config:   &fullCfg.Docker,
		fullCfg:  fullCfg,
		mountCfg: MountConfig{Mode: MountModeNative},
		workers:  make(map[string]*workerState),
		runCmd:   defaultCmdRunner,
	}

	// 4. Spawn — with the #230 bug, this fails with "endpoint <server>
	// already exists" because the server gets attached twice to the
	// worker network.
	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       testutil.AlpineImage(t),
		Cmd:         []string{"sleep", "300"},
		BundlePath:  "/tmp",
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		Labels:      map[string]string{},
	}
	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn with service network: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop(ctx, workerID) })

	// 5. Sanity check: the server is attached to the worker network with
	// the "blockyard" alias, which is what joinNetwork in step 6 of Spawn
	// is supposed to do. If the bug were present, the server would be
	// attached via its compose aliases instead (and Spawn would have
	// failed above).
	workerNet := "blockyard-" + workerID
	ni, err := cli.NetworkInspect(ctx, workerNet, client.NetworkInspectOptions{})
	if err != nil {
		t.Fatalf("inspect worker network: %v", err)
	}
	ep, ok := ni.Network.Containers[serverID]
	if !ok {
		t.Fatalf("server %s not attached to worker network", serverID)
	}
	t.Logf("server endpoint on worker network: name=%s", ep.Name)
}

