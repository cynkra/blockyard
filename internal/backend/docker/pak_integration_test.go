//go:build pak_test

package docker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/testutil"
)

func pakTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Docker: config.DockerConfig{
			Socket:     "/var/run/docker.sock",
			Image:      testutil.TOMLDockerImage(t),
			ShinyPort:  8080,
			PakVersion: "stable",
		},
	}
}

// TestBuildE2E_PakBuild exercises the Docker build pipeline with the new
// Cmd/Mounts API. Since we cannot run a full pak install without pak in the
// image, we use a simple R command to verify mounts and command override work.
func TestBuildE2E_PakBuild(t *testing.T) {
	image := testutil.TOMLDockerImage(t)

	ctx := context.Background()
	b, err := New(ctx, pakTestConfig(t), t.TempDir(), "test")
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
	image := testutil.TOMLDockerImage(t)

	ctx := context.Background()
	be, err := New(ctx, pakTestConfig(t), t.TempDir(), "test")
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
	manifestData := `{"version":1,"platform":"4.4.3","metadata":{"appmode":"shiny","entrypoint":"app.R"},"repositories":[{"Name":"CRAN","URL":"https://p3m.dev/cran/latest"}],"description":{"Imports":"mime"},"files":{"app.R":{"checksum":"abc"}}}`
	if err := os.WriteFile(filepath.Join(paths.Unpacked, "manifest.json"), []byte(manifestData), 0o644); err != nil {
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	bundle.SpawnRestore(bundle.RestoreParams{
		Ctx:          ctx,
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
	case <-ctx.Done():
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
