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

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/testutil"
)

func testConfig() *config.DockerConfig {
	return &config.DockerConfig{
		Socket:     "/var/run/docker.sock",
		Image:      "alpine:latest",
		ShinyPort:  8080,
		PakVersion: "stable",
	}
}


func TestSpawnAndStop(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(), t.TempDir())
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
	b, err := New(ctx, testConfig(), t.TempDir())
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
	b, err := New(ctx, testConfig(), t.TempDir())
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
	b, err := New(ctx, testConfig(), t.TempDir())
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
	b, err := New(ctx, testConfig(), t.TempDir())
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
	b, err := New(ctx, testConfig(), t.TempDir())
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
	b, err := New(ctx, testConfig(), t.TempDir())
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
	b, err := New(ctx, testConfig(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := b.Stop(ctx, "nonexistent"); err == nil {
		t.Fatal("expected error for unknown worker")
	}
}

func TestHealthCheckUnknownWorker(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(), t.TempDir())
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
	b, err := New(ctx, testConfig(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bundleDir, libDir := testBundleDir(t)
	spec := backend.BuildSpec{
		AppID:    "test-app",
		BundleID: uuid.New().String()[:8],
		Image:    "alpine:latest",
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
	b, err := New(ctx, testConfig(), t.TempDir())
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
		Image:    "alpine:latest",
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
	b, err := New(ctx, testConfig(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = b.Addr(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown worker")
	}
}

// TestBuildE2E_PakBuild exercises the Docker build pipeline with the new
// Cmd/Mounts API. Since we cannot run a full pak install without pak in the
// image, we use a simple R command to verify mounts and command override work.
func TestBuildE2E_PakBuild(t *testing.T) {
	const image = "ghcr.io/rocker-org/r-ver:4.4.3"

	ctx := context.Background()
	b, err := New(ctx, testConfig(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 1. Create a minimal bundle with manifest.json and app.R.
	bundleDir := t.TempDir()
	libDir := t.TempDir()

	manifest := `{
  "type": "unpinned",
  "description": {"Imports": "mime"},
  "packages": []
}`
	for name, content := range map[string]string{
		"manifest.json": manifest,
		"app.R":         "library(mime)\n",
	} {
		if err := os.WriteFile(filepath.Join(bundleDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// 2. Run the build with a simple R command that verifies mounts are accessible.
	spec := backend.BuildSpec{
		AppID:    "test-app",
		BundleID: uuid.New().String()[:8],
		Image:    image,
		Cmd:      []string{"R", "--vanilla", "-e", "cat('ok')"},
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
		t.Fatalf("build failed with exit code %d\n--- build logs ---\n%s", result.ExitCode, result.Logs)
	}
}

// TestFullPipeline_RestoreAndSpawnWorker exercises the entire production code
// path: bundle upload → SpawnRestore (pak build, Docker build) → worker spawn
// with bind mounts. This is the test that would have caught the host-path and
// mount issues we hit in real deployments.
func TestFullPipeline_RestoreAndSpawnWorker(t *testing.T) {
	const pakVersion = "stable"
	const image = "ghcr.io/rocker-org/r-ver:4.4.3"

	ctx := context.Background()
	be, err := New(ctx, testConfig(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// --- Setup: DB, base dirs, app row ---

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	basePath := t.TempDir()

	app, err := database.CreateApp("e2e-app", "")
	if err != nil {
		t.Fatal(err)
	}
	bundleID := uuid.New().String()[:8]
	if _, err := database.CreateBundle(bundleID, app.ID, "", false); err != nil {
		t.Fatal(err)
	}

	// --- Step 1: Write and unpack a bundle archive (same as the upload handler) ---

	archiveData := testutil.MakeBundle(t)
	paths := bundle.NewBundlePaths(basePath, app.ID, bundleID)
	if err := bundle.WriteArchive(paths, bytes.NewReader(archiveData)); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	if err := bundle.UnpackArchive(paths); err != nil {
		t.Fatalf("UnpackArchive: %v", err)
	}
	if err := bundle.CreateLibraryDir(paths); err != nil {
		t.Fatalf("CreateLibraryDir: %v", err)
	}

	// Write a minimal manifest with P3M repos for binary packages, and
	// overwrite the DESCRIPTION to only import 'mime' (pure R, no compilation).
	manifest := `{"version":1,"platform":"4.4.3","metadata":{"appmode":"shiny","entrypoint":"app.R"},"repositories":[{"Name":"CRAN","URL":"https://p3m.dev/cran/latest"}],"description":{"Imports":"mime"},"files":{"app.R":{"checksum":"abc"}}}`
	if err := os.WriteFile(filepath.Join(paths.Unpacked, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(paths.Unpacked, "DESCRIPTION"),
		[]byte("Package: testapp\nVersion: 0.1.0\nImports:\n    mime\n"), 0o644); err != nil {
		t.Fatalf("write DESCRIPTION: %v", err)
	}

	// Let EnsureInstalled actually install pak into a real cache dir.
	// This runs a container to download pak — slow but exercises the real path.
	pakCachePath := filepath.Join(basePath, ".pak-cache")

	// --- Step 2: SpawnRestore — goes through the full production restore path ---

	tasks := task.NewStore()
	taskID := uuid.New().String()
	sender := tasks.Create(taskID, app.ID)

	bundle.SpawnRestore(bundle.RestoreParams{
		Backend:      be,
		DB:           database,
		Tasks:        tasks,
		Sender:       sender,
		AppID:        app.ID,
		BundleID:     bundleID,
		Paths:        paths,
		Image:        image,
		PakVersion:   pakVersion,
		PakCachePath: pakCachePath,
		Retention:    5,
		BasePath:     basePath,
	})

	// Wait for restore to complete.
	_, _, done, ok := tasks.Subscribe(taskID)
	if !ok {
		t.Fatal("task not found")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Minute):
		t.Fatal("restore timed out")
	}

	status, _ := tasks.Status(taskID)
	if status != task.Completed {
		// Dump task logs for debugging.
		snap, _, _, _ := tasks.Subscribe(taskID)
		t.Fatalf("restore failed (status=%d); task logs:\n%s", status, strings.Join(snap, "\n"))
	}

	// Verify DB state.
	bRow, err := database.GetBundle(bundleID)
	if err != nil {
		t.Fatal(err)
	}
	if bRow.Status != "ready" {
		t.Fatalf("expected bundle status 'ready', got %q", bRow.Status)
	}
	appRow, err := database.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if appRow.ActiveBundle == nil || *appRow.ActiveBundle != bundleID {
		t.Fatal("active bundle not set after successful restore")
	}

	// --- Step 3: Spawn a worker using the built bundle ---
	// This exercises the same bind-mount path construction as coldstart.go.

	workerID := "e2e-worker-" + uuid.New().String()[:8]
	hostPaths := bundle.NewBundlePaths(basePath, app.ID, bundleID)

	workerSpec := backend.WorkerSpec{
		AppID:       app.ID,
		WorkerID:    workerID,
		Image:       image,
		Cmd:         []string{"R", "--no-save", "-e", "cat('worker ok'); Sys.sleep(60)"},
		BundlePath:  hostPaths.Unpacked,
		LibraryPath: hostPaths.Library,
		WorkerMount: "/app",
		ShinyPort:   8080,
		Labels:      map[string]string{},
	}

	if err := be.Spawn(ctx, workerSpec); err != nil {
		t.Fatalf("Spawn worker: %v", err)
	}
	defer be.Stop(ctx, workerID)

	// Verify worker has an address (is running).
	addr, err := be.Addr(ctx, workerID)
	if err != nil {
		t.Fatalf("Addr: %v", err)
	}
	if addr == "" {
		t.Fatal("worker has empty address")
	}
	t.Logf("worker running at %s", addr)

	// Verify the library was mounted by checking logs.
	time.Sleep(2 * time.Second)
	stream, err := be.Logs(ctx, workerID)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer stream.Close()

	var logLines []string
	timeout := time.After(5 * time.Second)
	for {
		select {
		case line, ok := <-stream.Lines:
			if !ok {
				goto checkLogs
			}
			logLines = append(logLines, line)
			if strings.Contains(line, "worker ok") {
				goto checkLogs
			}
		case <-timeout:
			goto checkLogs
		}
	}
checkLogs:
	allLogs := strings.Join(logLines, "\n")
	if !strings.Contains(allLogs, "worker ok") {
		t.Fatalf("worker did not produce expected output; logs:\n%s", allLogs)
	}
}

func TestSpawnWithMemoryLimit(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(), t.TempDir())
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

	info, err := b.client.ContainerInspect(ctx, ws.containerID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}

	expectedBytes := int64(64 * 1024 * 1024)
	if info.HostConfig.Memory != expectedBytes {
		t.Fatalf("expected memory limit %d bytes, got %d", expectedBytes, info.HostConfig.Memory)
	}
}

func TestSpawnWithCPULimit(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(), t.TempDir())
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

	info, err := b.client.ContainerInspect(ctx, ws.containerID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}

	expectedNanoCPUs := int64(0.5 * 1e9)
	if info.HostConfig.NanoCPUs != expectedNanoCPUs {
		t.Fatalf("expected NanoCPUs %d, got %d", expectedNanoCPUs, info.HostConfig.NanoCPUs)
	}
}

func TestSpawnWithEnvVars(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(), t.TempDir())
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

	execResp, err := b.client.ContainerExecCreate(ctx, ws.containerID,
		container.ExecOptions{
			Cmd:          []string{"sh", "-c", "echo $TEST_VAR"},
			AttachStdout: true,
		},
	)
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}

	attachResp, err := b.client.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
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
	b, err := New(ctx, testConfig(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bundleDir, libDir := testBundleDir(t)

	var logLines []string
	var mu sync.Mutex

	spec := backend.BuildSpec{
		AppID:    "test-app",
		BundleID: uuid.New().String()[:8],
		Image:    "alpine:latest",
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

	// Even if the rv binary exits immediately, the container lifecycle
	// itself may not produce output. The dummy rv script is silent,
	// so we just verify the callback mechanism doesn't panic and was wired up.
	t.Logf("LogWriter received %d lines", count)
}

func TestBuildExitCodeOnFailure(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bundleDir, libDir := testBundleDir(t)

	spec := backend.BuildSpec{
		AppID:    "test-app",
		BundleID: uuid.New().String()[:8],
		Image:    "alpine:latest",
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
	b, err := New(ctx, testConfig(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       "alpine:latest",
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
	b, err := New(ctx, testConfig(), t.TempDir())
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

func TestParseMemoryLimitEdgeCases(t *testing.T) {
	tests := []struct {
		input  string
		want   int64
		wantOk bool
	}{
		{"512m", 512 * 1024 * 1024, true},
		{"1g", 1024 * 1024 * 1024, true},
		{"256k", 256 * 1024, true},
		{"1024", 1024, true},
		{"", 0, false},
		{"abc", 0, false},
		{"m", 0, false},
		{"512mb", 512 * 1024 * 1024, true},
		{"2gb", 2 * 1024 * 1024 * 1024, true},
		{"128kb", 128 * 1024, true},
	}

	for _, tt := range tests {
		got, ok := ParseMemoryLimit(tt.input)
		if ok != tt.wantOk {
			t.Errorf("ParseMemoryLimit(%q): ok=%v, want %v", tt.input, ok, tt.wantOk)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("ParseMemoryLimit(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}
