package bundle

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mockmock "github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/testutil"
)

func TestWriteAndUnpackArchive(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeBundle(t)
	if err := WriteArchive(paths, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(paths.Archive); err != nil {
		t.Fatalf("archive not found: %v", err)
	}

	if err := UnpackArchive(paths); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(paths.Unpacked + "/app.R"); err != nil {
		t.Fatal("app.R not found in unpacked dir")
	}
}

func TestDeleteFiles(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeBundle(t)
	WriteArchive(paths, bytes.NewReader(data))
	UnpackArchive(paths)
	CreateLibraryDir(paths)

	DeleteFiles(paths)

	for _, p := range []string{paths.Archive, paths.Unpacked, paths.Library} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted", p)
		}
	}
}

func TestSetLibraryPath(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeBundle(t)
	WriteArchive(paths, bytes.NewReader(data))
	UnpackArchive(paths)

	// Write a realistic rproject.toml.
	config := "[project]\nname = \"test\"\nr_version = \"4.4\"\ndependencies = [\"shiny\"]\n"
	os.WriteFile(paths.Unpacked+"/rproject.toml", []byte(config), 0o644)

	if err := SetLibraryPath(paths, "/rv-library"); err != nil {
		t.Fatalf("SetLibraryPath: %v", err)
	}

	// Read back and verify.
	content, _ := os.ReadFile(paths.Unpacked + "/rproject.toml")
	if !strings.Contains(string(content), `library = "/rv-library"`) {
		t.Errorf("expected library directive, got:\n%s", content)
	}
	// Original fields should be preserved.
	if !strings.Contains(string(content), `name = "test"`) {
		t.Errorf("original config lost, got:\n%s", content)
	}
}

func TestSetLibraryPath_OverridesExisting(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeBundle(t)
	WriteArchive(paths, bytes.NewReader(data))
	UnpackArchive(paths)

	config := "library = \"/old/path\"\n[project]\nname = \"test\"\nr_version = \"4.4\"\n"
	os.WriteFile(paths.Unpacked+"/rproject.toml", []byte(config), 0o644)

	if err := SetLibraryPath(paths, "/rv-library"); err != nil {
		t.Fatalf("SetLibraryPath: %v", err)
	}

	content, _ := os.ReadFile(paths.Unpacked + "/rproject.toml")
	if !strings.Contains(string(content), `library = "/rv-library"`) {
		t.Errorf("expected new library directive, got:\n%s", content)
	}
	if strings.Contains(string(content), "/old/path") {
		t.Errorf("old library path should be replaced, got:\n%s", content)
	}
}

func TestSetLibraryPath_NoConfig(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeBundle(t)
	WriteArchive(paths, bytes.NewReader(data))
	UnpackArchive(paths)

	// No rproject.toml — should be a no-op.
	if err := SetLibraryPath(paths, "/rv-library"); err != nil {
		t.Fatalf("SetLibraryPath: %v", err)
	}
}

func TestPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeTraversalBundle(t)
	if err := WriteArchive(paths, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	err := UnpackArchive(paths)
	if err == nil {
		t.Fatal("expected error on path traversal")
	}
}

func TestWriteArchiveCreatesFiles(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeBundle(t)
	if err := WriteArchive(paths, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	// Archive file should exist
	info, err := os.Stat(paths.Archive)
	if err != nil {
		t.Fatalf("archive not created: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("archive is empty")
	}

	// App directory should have been created
	info, err = os.Stat(filepath.Dir(paths.Archive))
	if err != nil {
		t.Fatalf("app dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("app dir is not a directory")
	}
}

func TestUnpackArchiveInvalid(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	// Write a non-tar.gz file as the archive
	appDir := filepath.Dir(paths.Archive)
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Archive, []byte("this is not a gzip file"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := UnpackArchive(paths)
	if err == nil {
		t.Fatal("expected error unpacking invalid archive")
	}
}

func TestUnpackArchiveMissingFile(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	// Archive file does not exist
	err := UnpackArchive(paths)
	if err == nil {
		t.Fatal("expected error for missing archive")
	}
}

func TestDeleteFilesNonexistent(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "nonexistent-bundle")

	// Should not panic when paths don't exist
	DeleteFiles(paths)
}

func TestSetLibraryPathCreatesUpdatedConfig(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeBundle(t)
	WriteArchive(paths, bytes.NewReader(data))
	UnpackArchive(paths)

	// Write a config with existing library set to something else
	config := "[project]\nname = \"myapp\"\n"
	os.WriteFile(paths.Unpacked+"/rproject.toml", []byte(config), 0o644)

	if err := SetLibraryPath(paths, BuildContainerLibPath); err != nil {
		t.Fatalf("SetLibraryPath: %v", err)
	}

	content, _ := os.ReadFile(paths.Unpacked + "/rproject.toml")
	if !strings.Contains(string(content), `library = "/rv-library"`) {
		t.Errorf("expected library path in config, got:\n%s", content)
	}
}

func TestSetLibraryPathInvalidTOML(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeBundle(t)
	WriteArchive(paths, bytes.NewReader(data))
	UnpackArchive(paths)

	// Write invalid TOML
	os.WriteFile(paths.Unpacked+"/rproject.toml", []byte("{{invalid toml"), 0o644)

	err := SetLibraryPath(paths, "/rv-library")
	if err == nil {
		t.Fatal("expected error for invalid TOML")
	}
}

// ---------------------------------------------------------------------------
// ValidateEntrypoint
// ---------------------------------------------------------------------------

func TestValidateEntrypoint_Valid(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeBundle(t)
	if err := WriteArchive(paths, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := UnpackArchive(paths); err != nil {
		t.Fatal(err)
	}

	if err := ValidateEntrypoint(paths); err != nil {
		t.Fatalf("expected valid entrypoint, got: %v", err)
	}
}

func TestValidateEntrypoint_Missing(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	// Create unpacked dir without app.R
	if err := os.MkdirAll(paths.Unpacked, 0o755); err != nil {
		t.Fatal(err)
	}

	err := ValidateEntrypoint(paths)
	if err == nil {
		t.Fatal("expected error for missing entrypoint")
	}
}

// ---------------------------------------------------------------------------
// EnforceRetention
// ---------------------------------------------------------------------------

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestEnforceRetention_DeletesOldest(t *testing.T) {
	database := openTestDB(t)
	tmp := t.TempDir()

	app, err := database.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Create 4 bundles with staggered uploaded_at so ordering is deterministic.
	ids := make([]string, 4)
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("bundle-%d", i)
		ids[i] = id
		if _, err := database.CreateBundle(id, app.ID); err != nil {
			t.Fatal(err)
		}
		// Manually set uploaded_at so newest-first ordering is clear.
		ts := time.Date(2025, 1, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339)
		if _, err := database.Exec("UPDATE bundles SET uploaded_at = ? WHERE id = ?", ts, id); err != nil {
			t.Fatal(err)
		}
	}

	// Set bundle-3 (newest) as active.
	activeBundleID := ids[3]
	if err := database.SetActiveBundle(app.ID, activeBundleID); err != nil {
		t.Fatal(err)
	}

	// retention=2: newest-first is bundle-3 (active, kept), bundle-2 (slot 1),
	// bundle-1 (slot 2), bundle-0 (over limit, deleted).
	deleted := EnforceRetention(database, tmp, app.ID, activeBundleID, 2)

	if len(deleted) != 1 {
		t.Fatalf("expected 1 deletion, got %d: %v", len(deleted), deleted)
	}
	if deleted[0] != "bundle-0" {
		t.Fatalf("expected bundle-0 to be deleted, got %s", deleted[0])
	}

	remaining, err := database.ListBundlesByApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 3 {
		t.Fatalf("expected 3 remaining bundles, got %d", len(remaining))
	}
}

func TestEnforceRetention_ActiveBundlePreserved(t *testing.T) {
	database := openTestDB(t)
	tmp := t.TempDir()

	app, err := database.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Create 3 bundles. Make the oldest one active.
	ids := make([]string, 3)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("bundle-%d", i)
		ids[i] = id
		if _, err := database.CreateBundle(id, app.ID); err != nil {
			t.Fatal(err)
		}
		ts := time.Date(2025, 1, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339)
		if _, err := database.Exec("UPDATE bundles SET uploaded_at = ? WHERE id = ?", ts, id); err != nil {
			t.Fatal(err)
		}
	}

	// Active = oldest bundle (bundle-0). retention=1 keeps only 1 newest slot.
	activeBundleID := ids[0]
	deleted := EnforceRetention(database, tmp, app.ID, activeBundleID, 1)

	// bundle-2 is the newest (kept by retention). bundle-0 is active (kept).
	// bundle-1 should be deleted.
	if len(deleted) != 1 {
		t.Fatalf("expected 1 deletion, got %d: %v", len(deleted), deleted)
	}
	if deleted[0] != "bundle-1" {
		t.Fatalf("expected bundle-1 to be deleted, got %s", deleted[0])
	}

	// Verify active bundle still exists.
	b, err := database.GetBundle(activeBundleID)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		t.Fatal("active bundle was deleted")
	}
}

func TestEnforceRetention_UnderLimit(t *testing.T) {
	database := openTestDB(t)
	tmp := t.TempDir()

	app, err := database.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Create 2 bundles with retention=5 — nothing should be deleted.
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("bundle-%d", i)
		if _, err := database.CreateBundle(id, app.ID); err != nil {
			t.Fatal(err)
		}
	}

	deleted := EnforceRetention(database, tmp, app.ID, "bundle-1", 5)
	if len(deleted) != 0 {
		t.Fatalf("expected 0 deletions, got %d", len(deleted))
	}
}

// ---------------------------------------------------------------------------
// runRestore
// ---------------------------------------------------------------------------

func TestRunRestore_Success(t *testing.T) {
	database := openTestDB(t)
	tmp := t.TempDir()
	be := mockmock.New()
	be.BuildSuccess.Store(true)

	app, err := database.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateBundle("b-1", app.ID); err != nil {
		t.Fatal(err)
	}

	paths := NewBundlePaths(tmp, app.ID, "b-1")
	tasks := task.NewStore()
	sender := tasks.Create("task-1")

	params := RestoreParams{
		Backend:   be,
		DB:        database,
		Tasks:     tasks,
		Sender:    sender,
		AppID:     app.ID,
		BundleID:  "b-1",
		Paths:     paths,
		Image:     "test-image",
		RvVersion:    "4.4.0",
		RvBinaryPath: testutil.FakeRvBinary(t),
		Retention: 5,
		BasePath:  tmp,
	}

	if err := runRestore(params); err != nil {
		t.Fatalf("runRestore failed: %v", err)
	}

	// Bundle should be marked ready.
	b, err := database.GetBundle("b-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "ready" {
		t.Fatalf("expected status 'ready', got %q", b.Status)
	}

	// App should have active bundle set.
	appRow, err := database.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if appRow.ActiveBundle == nil || *appRow.ActiveBundle != "b-1" {
		t.Fatal("active bundle not set after successful restore")
	}
}

func TestRunRestore_BuildFailure(t *testing.T) {
	database := openTestDB(t)
	tmp := t.TempDir()
	be := mockmock.New()
	be.BuildSuccess.Store(false)

	app, err := database.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateBundle("b-1", app.ID); err != nil {
		t.Fatal(err)
	}

	paths := NewBundlePaths(tmp, app.ID, "b-1")
	tasks := task.NewStore()
	sender := tasks.Create("task-1")

	params := RestoreParams{
		Backend:   be,
		DB:        database,
		Tasks:     tasks,
		Sender:    sender,
		AppID:     app.ID,
		BundleID:  "b-1",
		Paths:     paths,
		Image:     "test-image",
		RvVersion:    "4.4.0",
		RvBinaryPath: testutil.FakeRvBinary(t),
		Retention: 5,
		BasePath:  tmp,
	}

	err = runRestore(params)
	if err == nil {
		t.Fatal("expected error from failed build")
	}

	// Bundle should still be in "building" status (runRestore sets it to
	// building before the build; the caller SpawnRestore sets it to failed).
	b, rerr := database.GetBundle("b-1")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if b.Status != "building" {
		t.Fatalf("expected status 'building', got %q", b.Status)
	}
}
