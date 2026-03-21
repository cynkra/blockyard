package bundle

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/audit"
	mockmock "github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/testutil"
)

// setupRestoreTest creates the common scaffolding for SpawnRestore tests:
// a DB with an app+bundle, unpacked bundle directory, and RestoreParams.
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
	if _, err := database.CreateBundle("b-1", app.ID); err != nil {
		t.Fatal(err)
	}

	paths := NewBundlePaths(tmp, app.ID, "b-1")

	// Write and unpack a bundle so the unpacked directory exists with app.R.
	data := testutil.MakeBundle(t)
	if err := WriteArchive(paths, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := UnpackArchive(paths); err != nil {
		t.Fatal(err)
	}
	// Create the library output directory.
	if err := CreateLibraryDir(paths); err != nil {
		t.Fatal(err)
	}
	// Write a minimal rproject.toml so SetLibraryPath succeeds.
	if err := os.WriteFile(paths.Unpacked+"/rproject.toml",
		[]byte("[project]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

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
		RvVersion:    "4.4.0",
		RvBinaryPath: testutil.FakeRvBinary(t),
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
