package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/pkgstore"
	"github.com/cynkra/blockyard/internal/task"
)

func setupRefreshTest(t *testing.T) *Server {
	t.Helper()
	srv, _ := testServerWithMock(t)

	// Set up store with a platform and a package.
	srv.PkgStore.SetPlatform("test-platform")
	pkgDir := srv.PkgStore.Path("shiny", "src1", "cfg1")
	os.MkdirAll(pkgDir, 0o755)
	os.WriteFile(filepath.Join(pkgDir, "DESCRIPTION"), []byte("Package: shiny"), 0o644)

	// Write config meta sidecar so Touch doesn't fail silently.
	metaPath := srv.PkgStore.ConfigMetaPath("shiny", "src1", "cfg1")
	os.WriteFile(metaPath, []byte(`{}`), 0o644)

	return srv
}

func TestRunRollback_PreviousRefresh(t *testing.T) {
	srv := setupRefreshTest(t)
	bundleID := "bundle-1"
	app := &db.AppRow{
		ID:           "app-1",
		ActiveBundle: &bundleID,
	}

	// Set up bundle directory with current and prev store-manifests.
	bundlePaths := srv.BundlePaths("app-1", bundleID)
	os.MkdirAll(bundlePaths.Base, 0o755)

	pkgstore.WriteStoreManifest(bundlePaths.Base, map[string]string{"shiny": "src2/cfg2"})
	// Write prev manifest.
	prevDir := t.TempDir()
	pkgstore.WriteStoreManifest(prevDir, map[string]string{"shiny": "src1/cfg1"})
	copyFile(
		filepath.Join(prevDir, "store-manifest.json"),
		filepath.Join(bundlePaths.Base, "store-manifest.json.prev"))

	sender := srv.Tasks.Create("task-1", "app-1")
	srv.RunRollback(context.Background(), app, "", sender)

	status, ok := srv.Tasks.Status("task-1")
	if !ok {
		t.Fatal("task not found")
	}
	if status != task.Completed {
		t.Errorf("expected Completed, got %v", status)
	}

	// The prev manifest should have been promoted to current.
	rolledBack, err := pkgstore.ReadStoreManifest(
		filepath.Join(bundlePaths.Base, "store-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack["shiny"] != "src1/cfg1" {
		t.Errorf("expected rolled back to src1/cfg1, got %q", rolledBack["shiny"])
	}
}

func TestRunRollback_ToBuild(t *testing.T) {
	srv := setupRefreshTest(t)
	bundleID := "bundle-1"
	app := &db.AppRow{
		ID:           "app-1",
		ActiveBundle: &bundleID,
	}

	bundlePaths := srv.BundlePaths("app-1", bundleID)
	os.MkdirAll(bundlePaths.Base, 0o755)

	pkgstore.WriteStoreManifest(bundlePaths.Base, map[string]string{"shiny": "src2/cfg2"})
	buildDir := t.TempDir()
	pkgstore.WriteStoreManifest(buildDir, map[string]string{"shiny": "src1/cfg1"})
	copyFile(
		filepath.Join(buildDir, "store-manifest.json"),
		filepath.Join(bundlePaths.Base, "store-manifest.json.build"))

	sender := srv.Tasks.Create("task-1", "app-1")
	srv.RunRollback(context.Background(), app, "build", sender)

	status, _ := srv.Tasks.Status("task-1")
	if status != task.Completed {
		t.Errorf("expected Completed, got %v", status)
	}

	rolledBack, err := pkgstore.ReadStoreManifest(
		filepath.Join(bundlePaths.Base, "store-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack["shiny"] != "src1/cfg1" {
		t.Errorf("expected rolled back to src1/cfg1, got %q", rolledBack["shiny"])
	}
}

func TestRunRollback_NoPrevManifest(t *testing.T) {
	srv := setupRefreshTest(t)
	bundleID := "bundle-1"
	app := &db.AppRow{
		ID:           "app-1",
		ActiveBundle: &bundleID,
	}

	bundlePaths := srv.BundlePaths("app-1", bundleID)
	os.MkdirAll(bundlePaths.Base, 0o755)
	pkgstore.WriteStoreManifest(bundlePaths.Base, map[string]string{"shiny": "src1/cfg1"})

	sender := srv.Tasks.Create("task-1", "app-1")
	srv.RunRollback(context.Background(), app, "", sender)

	status, _ := srv.Tasks.Status("task-1")
	if status != task.Failed {
		t.Errorf("expected Failed when no prev manifest, got %v", status)
	}
}

func TestRunRefresh_BuildFails(t *testing.T) {
	srv := setupRefreshTest(t)
	bundleID := "bundle-1"
	app := &db.AppRow{
		ID:           "app-1",
		ActiveBundle: &bundleID,
	}

	bundlePaths := srv.BundlePaths("app-1", bundleID)
	os.MkdirAll(bundlePaths.Unpacked, 0o755)

	m := &manifest.Manifest{
		Metadata: manifest.Metadata{Entrypoint: "app.R"},
	}

	// Make the build fail.
	srv.Backend.(*mock.MockBackend).BuildSuccess.Store(false)

	sender := srv.Tasks.Create("task-1", "app-1")
	changed := srv.RunRefresh(context.Background(), app, m, sender)

	if changed {
		t.Error("expected no change when build fails")
	}
	status, _ := srv.Tasks.Status("task-1")
	if status != task.Failed {
		t.Errorf("expected Failed, got %v", status)
	}
}

func TestDrainAndReplace(t *testing.T) {
	srv := setupRefreshTest(t)

	// Set up an old worker.
	srv.Workers.Set("old-w-000", ActiveWorker{AppID: "app-1", BundleID: "b-1"})
	srv.Registry.Set("old-w-000", "localhost:1234")

	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "store-manifest.json")
	pkgstore.WriteStoreManifest(tmpDir, map[string]string{"shiny": "src1/cfg1"})

	bundleID := "b-1"
	app := &db.AppRow{
		ID:           "app-1",
		ActiveBundle: &bundleID,
	}

	sender := srv.Tasks.Create("task-1", "app-1")
	srv.drainAndReplace(context.Background(), app, manifestPath, sender)

	// Old worker should be draining.
	if !srv.Workers.IsDraining("app-1") {
		t.Error("expected old workers to be draining")
	}

	// A new worker should have been spawned.
	count := srv.Workers.CountForApp("app-1")
	if count < 2 {
		t.Errorf("expected at least 2 workers (old draining + new), got %d", count)
	}
}

func TestLinkNewPackages(t *testing.T) {
	srv := setupRefreshTest(t)

	workerLibDir := t.TempDir()

	stageDir := t.TempDir()
	pkgstore.WriteStoreManifest(stageDir, map[string]string{"shiny": "src1/cfg1"})
	manifestPath := filepath.Join(stageDir, "store-manifest.json")

	err := srv.linkNewPackages(manifestPath, workerLibDir)
	if err != nil {
		t.Fatal(err)
	}

	linked := filepath.Join(workerLibDir, "shiny")
	if !dirExists(linked) {
		t.Error("expected shiny package to be linked into worker lib")
	}

	pm, err := pkgstore.ReadPackageManifest(workerLibDir)
	if err != nil {
		t.Fatal(err)
	}
	if pm["shiny"] != "src1/cfg1" {
		t.Errorf("package manifest: shiny = %q, want %q", pm["shiny"], "src1/cfg1")
	}
}

func TestLinkNewPackages_AlreadyLinked(t *testing.T) {
	srv := setupRefreshTest(t)

	workerLibDir := t.TempDir()
	pkgstore.WritePackageManifest(workerLibDir, map[string]string{"shiny": "src1/cfg1"})
	os.MkdirAll(filepath.Join(workerLibDir, "shiny"), 0o755)

	stageDir := t.TempDir()
	pkgstore.WriteStoreManifest(stageDir, map[string]string{"shiny": "src1/cfg1"})
	manifestPath := filepath.Join(stageDir, "store-manifest.json")

	err := srv.linkNewPackages(manifestPath, workerLibDir)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLinkNewPackages_MissingFromStore(t *testing.T) {
	srv := setupRefreshTest(t)

	workerLibDir := t.TempDir()
	stageDir := t.TempDir()
	pkgstore.WriteStoreManifest(stageDir, map[string]string{
		"nonexistent": "missing/hash",
	})
	manifestPath := filepath.Join(stageDir, "store-manifest.json")

	err := srv.linkNewPackages(manifestPath, workerLibDir)
	if err == nil {
		t.Error("expected error for package not in store")
	}
}
