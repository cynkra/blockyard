//go:build docker_test

package docker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
)

func testConfig() *config.DockerConfig {
	return &config.DockerConfig{
		Socket:    "/var/run/docker.sock",
		Image:     "alpine:latest",
		ShinyPort: 8080,
		RvVersion: "latest",
	}
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

func TestBuildSuccess(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tmp := t.TempDir()
	spec := backend.BuildSpec{
		AppID:     "test-app",
		BundleID:  uuid.New().String()[:8],
		Image:     "alpine:latest",
		RvVersion: "latest",
		BundlePath:  tmp,
		LibraryPath: tmp,
		Labels:    map[string]string{},
	}

	// Build will fail because rv download won't work, but it exercises
	// the full create->start->wait->remove flow. We just check it
	// returns a result with a non-zero exit code.
	result, err := b.Build(ctx, spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// curl/rv won't be available in alpine, so expect failure
	if result.Success {
		t.Error("expected build to fail in bare alpine (no curl)")
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
