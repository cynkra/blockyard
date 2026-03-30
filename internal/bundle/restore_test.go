package bundle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/audit"
	mockmock "github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/task"
)

// setupRestoreTest creates the common scaffolding for SpawnRestore tests:
// a DB with an app+bundle, unpacked bundle directory with manifest, and RestoreParams.
func setupRestoreTest(t *testing.T, buildSuccess bool) (RestoreParams, *task.Store) {
	t.Helper()

	database := openTestDB(t)
	tmp := t.TempDir()
	be := mockmock.New()
	be.BuildSuccess.Store(buildSuccess)

	app, err := database.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateBundle("b-1", app.ID, "", false); err != nil {
		t.Fatal(err)
	}

	paths := NewBundlePaths(tmp, app.ID, "b-1")

	// Create unpacked dir with app.R and manifest.json.
	os.MkdirAll(paths.Unpacked, 0o755)
	os.MkdirAll(paths.Library, 0o755)
	os.WriteFile(filepath.Join(paths.Unpacked, "app.R"),
		[]byte("library(shiny)\nshinyApp(ui, server)"), 0o644)

	m := manifest.Manifest{
		Version:  1,
		Platform: "4.4.2",
		Metadata: manifest.Metadata{AppMode: "shiny", Entrypoint: "app.R"},
		Description: map[string]string{
			"Imports": "shiny",
		},
		Files: map[string]manifest.FileInfo{
			"app.R": {Checksum: "abc123"},
		},
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(filepath.Join(paths.Unpacked, "manifest.json"), data, 0o644)

	// Pre-create a fake pak cache dir so EnsureInstalled finds it.
	pakCacheDir := filepath.Join(tmp, ".pak-cache")
	os.MkdirAll(filepath.Join(pakCacheDir, "pak-stable"), 0o755)

	tasks := task.NewStore()
	sender := tasks.Create("task-1", "")

	params := RestoreParams{
		Backend:      be,
		DB:           database,
		Tasks:        tasks,
		Sender:       sender,
		AppID:        app.ID,
		BundleID:     "b-1",
		Paths:        paths,
		Image:        "test-image",
		PakVersion:   "stable",
		PakCachePath: pakCacheDir,
		Retention:    5,
		BasePath:     tmp,
	}

	return params, tasks
}

func TestSpawnRestore_BuildFailure(t *testing.T) {
	params, tasks := setupRestoreTest(t, false)

	SpawnRestore(params)

	// Wait for the background goroutine to finish via the task's done channel.
	_, _, done, ok := tasks.Subscribe("task-1")
	if !ok {
		t.Fatal("task not found")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SpawnRestore to complete")
	}

	// Task should be marked failed.
	status, ok := tasks.Status("task-1")
	if !ok {
		t.Fatal("task not found after completion")
	}
	if status != task.Failed {
		t.Fatalf("expected task status Failed, got %d", status)
	}

	// Bundle status in DB should be "failed".
	b, err := params.DB.GetBundle("b-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "failed" {
		t.Fatalf("expected bundle status 'failed', got %q", b.Status)
	}

	// App should NOT have an active bundle (build failed, never activated).
	appRow, err := params.DB.GetApp(params.AppID)
	if err != nil {
		t.Fatal(err)
	}
	if appRow.ActiveBundle != nil {
		t.Fatalf("expected no active bundle, got %q", *appRow.ActiveBundle)
	}
}

func TestSpawnRestore_Success(t *testing.T) {
	params, tasks := setupRestoreTest(t, true)

	SpawnRestore(params)

	// Wait for the background goroutine to finish via the task's done channel.
	_, _, done, ok := tasks.Subscribe("task-1")
	if !ok {
		t.Fatal("task not found")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SpawnRestore to complete")
	}

	// Task should be marked completed.
	status, ok := tasks.Status("task-1")
	if !ok {
		t.Fatal("task not found after completion")
	}
	if status != task.Completed {
		t.Fatalf("expected task status Completed, got %d", status)
	}

	// Bundle status in DB should be "ready".
	b, err := params.DB.GetBundle("b-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "ready" {
		t.Fatalf("expected bundle status 'ready', got %q", b.Status)
	}

	// App should have the bundle set as active.
	appRow, err := params.DB.GetApp(params.AppID)
	if err != nil {
		t.Fatal(err)
	}
	if appRow.ActiveBundle == nil || *appRow.ActiveBundle != "b-1" {
		t.Fatal("expected active bundle to be 'b-1'")
	}
}

func TestSpawnRestore_SuccessWithAuditLog(t *testing.T) {
	params, tasks := setupRestoreTest(t, true)

	// Set up audit logging.
	auditPath := t.TempDir() + "/audit.jsonl"
	auditLog := audit.New(auditPath)
	params.AuditLog = auditLog
	params.AuditActor = "test-user"

	SpawnRestore(params)

	_, _, done, ok := tasks.Subscribe("task-1")
	if !ok {
		t.Fatal("task not found")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SpawnRestore to complete")
	}

	status, ok := tasks.Status("task-1")
	if !ok || status != task.Completed {
		t.Fatalf("expected Completed, got %d", status)
	}
}

func TestSpawnRestore_FailureWithAuditLog(t *testing.T) {
	params, tasks := setupRestoreTest(t, false)

	// Set up audit logging.
	auditPath := t.TempDir() + "/audit.jsonl"
	auditLog := audit.New(auditPath)
	params.AuditLog = auditLog
	params.AuditActor = "test-user"

	SpawnRestore(params)

	_, _, done, ok := tasks.Subscribe("task-1")
	if !ok {
		t.Fatal("task not found")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SpawnRestore to complete")
	}

	status, ok := tasks.Status("task-1")
	if !ok || status != task.Failed {
		t.Fatalf("expected Failed, got %d", status)
	}
}

func TestBuildCommand(t *testing.T) {
	cmd := BuildCommand()
	if len(cmd) != 4 {
		t.Fatalf("expected 4 parts, got %d", len(cmd))
	}
	if cmd[0] != "Rscript" || cmd[1] != "--vanilla" || cmd[2] != "-e" {
		t.Errorf("prefix = %v", cmd[:3])
	}
	// The R script should reference the by-builder store commands.
	if len(cmd[3]) < 100 {
		t.Error("R script appears empty or truncated")
	}
}

func TestBuildMounts(t *testing.T) {
	mounts := BuildMounts("/pak", "/app", "/store", "/cache", "/tools/by-builder")
	if len(mounts) != 5 {
		t.Fatalf("expected 5 mounts, got %d", len(mounts))
	}
	// Verify critical mount: store must be read-write.
	for _, m := range mounts {
		if m.Target == "/store" {
			if m.ReadOnly {
				t.Error("store mount should be read-write")
			}
			if m.Source != "/store" {
				t.Errorf("store source = %q", m.Source)
			}
			return
		}
	}
	t.Error("store mount not found")
}
