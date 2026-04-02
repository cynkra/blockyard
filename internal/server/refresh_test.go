package server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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

	// Set PakVersion and pre-create pak cache so EnsureInstalled skips its Build.
	srv.Config.Docker.PakVersion = "stable"
	bsp := srv.Config.Storage.BundleServerPath
	os.MkdirAll(filepath.Join(bsp, ".pak-cache", "pak-stable"), 0o755)

	// Pre-create builder cache so EnsureCached skips compilation.
	builderCacheDir := filepath.Join(bsp, ".by-builder-cache")
	os.MkdirAll(builderCacheDir, 0o755)
	// Create a fake builder binary matching the expected name.
	fakeBuilder := filepath.Join(builderCacheDir, "by-builder-"+srv.Version+"-linux-"+runtime.GOARCH)
	os.WriteFile(fakeBuilder, []byte("#!/bin/sh\n"), 0o755)

	// Make the refresh build itself fail.
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
	err := srv.drainAndReplace(context.Background(), app, manifestPath, sender)
	if err != nil {
		t.Fatalf("drainAndReplace: %v", err)
	}

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

func TestDrainAndReplace_HealthCheckFails(t *testing.T) {
	srv, be := testServerWithMock(t)

	// Set up store with a package so AssembleLibrary succeeds.
	srv.PkgStore.SetPlatform("test-platform")
	pkgDir := srv.PkgStore.Path("shiny", "src1", "cfg1")
	os.MkdirAll(pkgDir, 0o755)
	os.WriteFile(filepath.Join(pkgDir, "DESCRIPTION"), []byte("Package: shiny"), 0o644)
	metaPath := srv.PkgStore.ConfigMetaPath("shiny", "src1", "cfg1")
	os.WriteFile(metaPath, []byte(`{}`), 0o644)

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

	// Make health check fail.
	be.HealthOK.Store(false)

	sender := srv.Tasks.Create("task-1", "app-1")
	err := srv.drainAndReplace(context.Background(), app, manifestPath, sender)
	if err == nil {
		t.Fatal("expected drainAndReplace to return an error")
	}

	// Old worker should NOT be draining — restored after failure.
	if srv.Workers.IsDraining("app-1") {
		t.Error("old workers should be un-drained after health-check failure")
	}

	// Only the old worker should remain.
	count := srv.Workers.CountForApp("app-1")
	if count != 1 {
		t.Errorf("expected 1 worker (old restored), got %d", count)
	}

	// Old worker should still be accessible.
	w, ok := srv.Workers.Get("old-w-000")
	if !ok {
		t.Fatal("old worker should still exist")
	}
	if w.Draining {
		t.Error("old worker should not be draining")
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

func TestStoreManifestsChanged_SameContent(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	pkgstore.WriteStoreManifest(dir1, map[string]string{"shiny": "src1/cfg1", "dplyr": "src2/cfg2"})
	pkgstore.WriteStoreManifest(dir2, map[string]string{"shiny": "src1/cfg1", "dplyr": "src2/cfg2"})

	changed, err := storeManifestsChanged(
		filepath.Join(dir1, "store-manifest.json"),
		filepath.Join(dir2, "store-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("expected no change for identical manifests")
	}
}

func TestStoreManifestsChanged_DifferentLength(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	pkgstore.WriteStoreManifest(dir1, map[string]string{"shiny": "src1/cfg1"})
	pkgstore.WriteStoreManifest(dir2, map[string]string{"shiny": "src1/cfg1", "dplyr": "src2/cfg2"})

	changed, err := storeManifestsChanged(
		filepath.Join(dir1, "store-manifest.json"),
		filepath.Join(dir2, "store-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("expected change for different-length manifests")
	}
}

func TestStoreManifestsChanged_DifferentValues(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	pkgstore.WriteStoreManifest(dir1, map[string]string{"shiny": "src1/cfg1"})
	pkgstore.WriteStoreManifest(dir2, map[string]string{"shiny": "src2/cfg2"})

	changed, err := storeManifestsChanged(
		filepath.Join(dir1, "store-manifest.json"),
		filepath.Join(dir2, "store-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("expected change for different values")
	}
}

func TestStoreManifestsChanged_OldMissing(t *testing.T) {
	dir := t.TempDir()
	pkgstore.WriteStoreManifest(dir, map[string]string{"shiny": "src1/cfg1"})

	_, err := storeManifestsChanged(
		filepath.Join(dir, "nonexistent.json"),
		filepath.Join(dir, "store-manifest.json"))
	if err == nil {
		t.Error("expected error for missing old manifest")
	}
}

func TestRunRefresh_PakInstallFails(t *testing.T) {
	srv := setupRefreshTest(t)
	// Use an invalid pak version to make EnsureInstalled fail.
	srv.Config.Docker.PakVersion = "invalid-version"

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

	sender := srv.Tasks.Create("task-1", "app-1")
	changed := srv.RunRefresh(context.Background(), app, m, sender)

	if changed {
		t.Error("expected no change when pak install fails")
	}
	status, _ := srv.Tasks.Status("task-1")
	if status != task.Failed {
		t.Errorf("expected Failed, got %v", status)
	}
}

func TestRunRollback_NoBuildManifest(t *testing.T) {
	srv := setupRefreshTest(t)
	bundleID := "bundle-1"
	app := &db.AppRow{
		ID:           "app-1",
		ActiveBundle: &bundleID,
	}

	bundlePaths := srv.BundlePaths("app-1", bundleID)
	os.MkdirAll(bundlePaths.Base, 0o755)
	pkgstore.WriteStoreManifest(bundlePaths.Base, map[string]string{"shiny": "src1/cfg1"})

	// No .build manifest exists.
	sender := srv.Tasks.Create("task-1", "app-1")
	srv.RunRollback(context.Background(), app, "build", sender)

	status, _ := srv.Tasks.Status("task-1")
	if status != task.Failed {
		t.Errorf("expected Failed when build manifest missing, got %v", status)
	}
}

func TestLinkNewPackages_ReplacesDifferentRef(t *testing.T) {
	srv := setupRefreshTest(t)

	// Add a second package to the store.
	pkgDir2 := srv.PkgStore.Path("shiny", "src2", "cfg2")
	os.MkdirAll(pkgDir2, 0o755)
	os.WriteFile(filepath.Join(pkgDir2, "DESCRIPTION"), []byte("Package: shiny"), 0o644)
	metaPath2 := srv.PkgStore.ConfigMetaPath("shiny", "src2", "cfg2")
	os.WriteFile(metaPath2, []byte(`{}`), 0o644)

	workerLibDir := t.TempDir()
	// Pre-populate with old ref.
	pkgstore.WritePackageManifest(workerLibDir, map[string]string{"shiny": "src1/cfg1"})
	os.MkdirAll(filepath.Join(workerLibDir, "shiny"), 0o755)

	stageDir := t.TempDir()
	pkgstore.WriteStoreManifest(stageDir, map[string]string{"shiny": "src2/cfg2"})
	manifestPath := filepath.Join(stageDir, "store-manifest.json")

	err := srv.linkNewPackages(manifestPath, workerLibDir)
	if err != nil {
		t.Fatal(err)
	}

	pm, _ := pkgstore.ReadPackageManifest(workerLibDir)
	if pm["shiny"] != "src2/cfg2" {
		t.Errorf("package manifest: shiny = %q, want %q", pm["shiny"], "src2/cfg2")
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
