package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cynkra/blockyard/internal/pkgstore"
)

func writeTestStoreManifest(t *testing.T, dir string, manifest map[string]string) string {
	t.Helper()
	if err := pkgstore.WriteStoreManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "store-manifest.json")
}

func TestDetectConflict_NoConflict(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestStoreManifest(t, dir, map[string]string{
		"DT": "abc123/config1",
	})

	// DT is new (not in worker manifest) and not loaded.
	workerManifest := map[string]string{}
	loaded := []string{"shiny"}

	conflict, _, err := detectConflict(manifestPath, workerManifest, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		t.Error("expected no conflict for new package")
	}
}

func TestDetectConflict_SameCompoundRef(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestStoreManifest(t, dir, map[string]string{
		"shiny": "abc123/config1",
	})

	workerManifest := map[string]string{
		"shiny": "abc123/config1",
	}
	loaded := []string{"shiny"}

	conflict, _, err := detectConflict(manifestPath, workerManifest, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		t.Error("expected no conflict when compound refs match")
	}
}

func TestDetectConflict_DifferentSourceHash(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestStoreManifest(t, dir, map[string]string{
		"shiny": "newversion/config1",
	})

	workerManifest := map[string]string{
		"shiny": "oldversion/config1",
	}
	loaded := []string{"shiny"}

	conflict, pkg, err := detectConflict(manifestPath, workerManifest, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !conflict {
		t.Error("expected conflict when source hash differs")
	}
	if pkg != "shiny" {
		t.Errorf("conflict package = %q, want %q", pkg, "shiny")
	}
}

func TestDetectConflict_SameSourceDifferentConfig(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestStoreManifest(t, dir, map[string]string{
		"sf": "abc123/newconfig",
	})

	workerManifest := map[string]string{
		"sf": "abc123/oldconfig",
	}
	loaded := []string{"sf"}

	conflict, pkg, err := detectConflict(manifestPath, workerManifest, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !conflict {
		t.Error("expected conflict when config hash differs (ABI change)")
	}
	if pkg != "sf" {
		t.Errorf("conflict package = %q, want %q", pkg, "sf")
	}
}

func TestDetectConflict_NotLoaded(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestStoreManifest(t, dir, map[string]string{
		"DT": "newversion/config1",
	})

	workerManifest := map[string]string{
		"DT": "oldversion/config1",
	}
	// DT is installed but NOT loaded — no conflict.
	loaded := []string{"shiny"}

	conflict, _, err := detectConflict(manifestPath, workerManifest, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		t.Error("expected no conflict when package is not loaded")
	}
}

func TestDetectConflict_NotInNewManifest(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeTestStoreManifest(t, dir, map[string]string{
		"DT": "abc123/config1",
	})

	workerManifest := map[string]string{
		"shiny": "old/config",
	}
	// shiny is loaded but not in the new manifest → no conflict.
	loaded := []string{"shiny"}

	conflict, _, err := detectConflict(manifestPath, workerManifest, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		t.Error("expected no conflict when loaded package not in new manifest")
	}
}

func TestStoreManifestsChanged_Identical(t *testing.T) {
	dir := t.TempDir()
	m := map[string]string{"shiny": "abc/def", "DT": "ghi/jkl"}
	path1 := filepath.Join(dir, "old")
	path2 := filepath.Join(dir, "new")
	os.MkdirAll(path1, 0o755)
	os.MkdirAll(path2, 0o755)
	pkgstore.WriteStoreManifest(path1, m)
	pkgstore.WriteStoreManifest(path2, m)

	changed, err := storeManifestsChanged(
		filepath.Join(path1, "store-manifest.json"),
		filepath.Join(path2, "store-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("expected no change for identical manifests")
	}
}

func TestStoreManifestsChanged_VersionBump(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old")
	new := filepath.Join(dir, "new")
	os.MkdirAll(old, 0o755)
	os.MkdirAll(new, 0o755)
	pkgstore.WriteStoreManifest(old, map[string]string{"shiny": "v1/c1"})
	pkgstore.WriteStoreManifest(new, map[string]string{"shiny": "v2/c1"})

	changed, err := storeManifestsChanged(
		filepath.Join(old, "store-manifest.json"),
		filepath.Join(new, "store-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("expected change for version bump")
	}
}

func TestStoreManifestsChanged_PackageAdded(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old")
	new := filepath.Join(dir, "new")
	os.MkdirAll(old, 0o755)
	os.MkdirAll(new, 0o755)
	pkgstore.WriteStoreManifest(old, map[string]string{"shiny": "v1/c1"})
	pkgstore.WriteStoreManifest(new, map[string]string{"shiny": "v1/c1", "DT": "v1/c1"})

	changed, err := storeManifestsChanged(
		filepath.Join(old, "store-manifest.json"),
		filepath.Join(new, "store-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("expected change when package added")
	}
}
