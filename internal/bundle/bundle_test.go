package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mockmock "github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/manifest"
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

func TestValidateEntrypoint_ServerR_Rejected(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	// server.R alone is not a valid entrypoint — must be app.R.
	os.MkdirAll(paths.Unpacked, 0o755)
	os.WriteFile(filepath.Join(paths.Unpacked, "server.R"), []byte("# server"), 0o644)

	if err := ValidateEntrypoint(paths); err == nil {
		t.Fatal("expected error for server.R without app.R")
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
	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
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
		if _, err := database.CreateBundle(id, app.ID, "", false); err != nil {
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
		if _, err := database.CreateBundle(id, app.ID, "", false); err != nil {
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
		if _, err := database.CreateBundle(id, app.ID, "", false); err != nil {
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

// writeBundleWithManifest creates an unpacked bundle with app.R and a manifest.
func writeBundleWithManifest(t *testing.T, paths Paths) {
	t.Helper()
	os.MkdirAll(paths.Unpacked, 0o755)
	os.MkdirAll(paths.Library, 0o755)
	os.WriteFile(filepath.Join(paths.Unpacked, "app.R"),
		[]byte("library(shiny)\nshinyApp(ui, server)"), 0o644)

	m := manifest.Manifest{
		Version:  1,
		RVersion: "4.4.2",
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
}

func TestRunRestore_Success(t *testing.T) {
	database := openTestDB(t)
	tmp := t.TempDir()
	be := mockmock.New()
	be.BuildSuccess.Store(true)

	app, err := database.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateBundle("b-1", app.ID, "", false); err != nil {
		t.Fatal(err)
	}

	paths := NewBundlePaths(tmp, app.ID, "b-1")
	writeBundleWithManifest(t, paths)
	tasks := task.NewStore()
	sender := tasks.Create("task-1", "")

	// Pre-create a fake pak cache dir so EnsureInstalled finds it.
	pakCacheDir := filepath.Join(tmp, ".pak-cache")
	os.MkdirAll(filepath.Join(pakCacheDir, "pak-stable"), 0o755)

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
	if _, err := database.CreateBundle("b-1", app.ID, "", false); err != nil {
		t.Fatal(err)
	}

	paths := NewBundlePaths(tmp, app.ID, "b-1")
	writeBundleWithManifest(t, paths)
	tasks := task.NewStore()
	sender := tasks.Create("task-1", "")

	pakCacheDir := filepath.Join(tmp, ".pak-cache")
	os.MkdirAll(filepath.Join(pakCacheDir, "pak-stable"), 0o755)

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

func TestUnpackArchiveRejectsSymlinks(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	// Build a tar.gz containing a symlink entry.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// Add a regular file first so the archive isn't empty.
	tw.WriteHeader(&tar.Header{Name: "app.R", Size: 6, Typeflag: tar.TypeReg})
	tw.Write([]byte("# app\n"))

	// Add a symlink entry — should be rejected.
	tw.WriteHeader(&tar.Header{
		Name:     "evil-link",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
	})

	tw.Close()
	gw.Close()

	if err := WriteArchive(paths, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}

	err := UnpackArchive(paths)
	if err == nil {
		t.Fatal("expected error for archive containing symlink")
	}
	if !strings.Contains(err.Error(), "unsupported link") {
		t.Errorf("expected 'unsupported link' error, got: %v", err)
	}
}

func TestWriteArchiveReaderError(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	err := WriteArchive(paths, &errorReader{})
	if err == nil {
		t.Fatal("expected error from reader")
	}
	if !strings.Contains(err.Error(), "write archive") {
		t.Errorf("expected 'write archive' error, got: %v", err)
	}

	// Temp file should have been cleaned up.
	entries, _ := os.ReadDir(filepath.Dir(paths.Archive))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file %q was not cleaned up", e.Name())
		}
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("simulated read error")
}
