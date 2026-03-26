package pkgstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadPackageManifest(t *testing.T) {
	dir := t.TempDir()
	manifest := map[string]string{
		"shiny":   "src1/cfg1",
		"ggplot2": "src2/cfg2",
	}
	if err := WritePackageManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}

	got, err := ReadPackageManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got["shiny"] != "src1/cfg1" {
		t.Errorf("shiny = %q", got["shiny"])
	}
	if got["ggplot2"] != "src2/cfg2" {
		t.Errorf("ggplot2 = %q", got["ggplot2"])
	}
}

func TestUpdatePackageManifest(t *testing.T) {
	dir := t.TempDir()

	// Write initial manifest.
	WritePackageManifest(dir, map[string]string{
		"shiny": "src1/cfg1",
	})

	// Update with new entry.
	if err := UpdatePackageManifest(dir, map[string]string{
		"ggplot2": "src2/cfg2",
	}); err != nil {
		t.Fatal(err)
	}

	got, _ := ReadPackageManifest(dir)
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d", len(got))
	}
	if got["shiny"] != "src1/cfg1" {
		t.Error("original entry should be preserved")
	}
	if got["ggplot2"] != "src2/cfg2" {
		t.Error("new entry should be added")
	}
}

func TestUpdatePackageManifest_Overwrite(t *testing.T) {
	dir := t.TempDir()

	WritePackageManifest(dir, map[string]string{
		"shiny": "src1/cfg1",
	})

	// Overwrite existing key.
	if err := UpdatePackageManifest(dir, map[string]string{
		"shiny": "src2/cfg2",
	}); err != nil {
		t.Fatal(err)
	}

	got, _ := ReadPackageManifest(dir)
	if got["shiny"] != "src2/cfg2" {
		t.Errorf("shiny = %q (expected overwritten value)", got["shiny"])
	}
}

func TestWriteReadStoreManifest(t *testing.T) {
	dir := t.TempDir()
	manifest := map[string]string{
		"shiny": "src1/cfg1",
	}
	if err := WriteStoreManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "store-manifest.json")
	got, err := ReadStoreManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["shiny"] != "src1/cfg1" {
		t.Errorf("shiny = %q", got["shiny"])
	}
}

func TestReadStoreManifest_NotFound(t *testing.T) {
	_, err := ReadStoreManifest("/nonexistent/store-manifest.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestUpdatePackageManifest_NoExistingFile(t *testing.T) {
	dir := t.TempDir()
	// No prior manifest — should create from scratch.
	if err := UpdatePackageManifest(dir, map[string]string{
		"shiny": "src1/cfg1",
	}); err != nil {
		t.Fatal(err)
	}

	got, _ := ReadPackageManifest(dir)
	if got["shiny"] != "src1/cfg1" {
		t.Errorf("shiny = %q", got["shiny"])
	}
}

func TestReadPackageManifest_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadPackageManifest(dir)
	if err == nil {
		t.Error("expected error for missing file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected IsNotExist error, got %v", err)
	}
}
