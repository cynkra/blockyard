package bundle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cynkra/blockyard/internal/manifest"
)

func writeManifest(t *testing.T, dir string, m manifest.Manifest) {
	t.Helper()
	data, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644)
}

func TestResolveManifest_ManifestExists(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	m := manifest.Manifest{
		Version:  1,
		Platform: "4.4.2",
		Metadata: manifest.Metadata{AppMode: "shiny", Entrypoint: "app.R"},
		Packages: map[string]manifest.Package{
			"shiny": {Package: "shiny", Version: "1.9.1", Source: "Repository"},
		},
		Files: map[string]manifest.FileInfo{"app.R": {Checksum: "abc"}},
	}
	writeManifest(t, dir, m)

	got, err := resolveManifest(dir)
	if err != nil {
		t.Fatalf("resolveManifest: %v", err)
	}
	if !got.IsPinned() {
		t.Error("expected pinned manifest")
	}
	if got.Packages["shiny"].Version != "1.9.1" {
		t.Errorf("shiny version = %q", got.Packages["shiny"].Version)
	}
}

func TestResolveManifest_RenvLock(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	lock := map[string]any{
		"R": map[string]any{
			"Version":      "4.4.2",
			"Repositories": []map[string]string{},
		},
		"Packages": map[string]map[string]string{
			"shiny": {"Package": "shiny", "Version": "1.9.1", "Source": "Repository"},
		},
	}
	data, _ := json.MarshalIndent(lock, "", "  ")
	os.WriteFile(filepath.Join(dir, "renv.lock"), data, 0o644)

	got, err := resolveManifest(dir)
	if err != nil {
		t.Fatalf("resolveManifest: %v", err)
	}
	if !got.IsPinned() {
		t.Error("expected pinned manifest from renv.lock")
	}
}

func TestResolveManifest_Description(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)
	os.WriteFile(filepath.Join(dir, "DESCRIPTION"),
		[]byte("Package: myapp\nImports: shiny\n"), 0o644)

	got, err := resolveManifest(dir)
	if err != nil {
		t.Fatalf("resolveManifest: %v", err)
	}
	if got.IsPinned() {
		t.Error("expected unpinned manifest from DESCRIPTION")
	}
	if got.Description["Imports"] != "shiny" {
		t.Errorf("Imports = %q", got.Description["Imports"])
	}
}

func TestResolveManifest_BareScripts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("library(shiny)"), 0o644)

	got, err := resolveManifest(dir)
	if err != nil {
		t.Fatalf("resolveManifest: %v", err)
	}
	if got != nil {
		t.Error("expected nil for bare scripts (needs pre-processing)")
	}
}

func TestResolveManifest_Priority(t *testing.T) {
	// manifest.json should win over renv.lock and DESCRIPTION.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)
	os.WriteFile(filepath.Join(dir, "DESCRIPTION"),
		[]byte("Package: myapp\nImports: shiny\n"), 0o644)

	// Write an renv.lock too.
	lock := map[string]any{
		"R": map[string]any{
			"Version":      "4.4.2",
			"Repositories": []map[string]string{},
		},
		"Packages": map[string]map[string]string{
			"ggplot2": {"Package": "ggplot2", "Version": "3.5.0", "Source": "Repository"},
		},
	}
	data, _ := json.MarshalIndent(lock, "", "  ")
	os.WriteFile(filepath.Join(dir, "renv.lock"), data, 0o644)

	// Write a manifest — this should take priority.
	m := manifest.Manifest{
		Version:  1,
		Metadata: manifest.Metadata{Entrypoint: "app.R"},
		Packages: map[string]manifest.Package{
			"DT": {Package: "DT", Version: "0.30", Source: "Repository"},
		},
		Files: map[string]manifest.FileInfo{},
	}
	writeManifest(t, dir, m)

	got, err := resolveManifest(dir)
	if err != nil {
		t.Fatalf("resolveManifest: %v", err)
	}
	// Should have DT (from manifest), not ggplot2 (from renv.lock).
	if _, ok := got.Packages["DT"]; !ok {
		t.Error("expected manifest.json to take priority")
	}
}

func TestDetectEntrypoint(t *testing.T) {
	dir := t.TempDir()

	// No files → default app.R
	if got := detectEntrypoint(dir); got != "app.R" {
		t.Errorf("default = %q", got)
	}

	// server.R present
	os.WriteFile(filepath.Join(dir, "server.R"), []byte("# server"), 0o644)
	if got := detectEntrypoint(dir); got != "server.R" {
		t.Errorf("with server.R = %q", got)
	}

	// app.R wins
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)
	if got := detectEntrypoint(dir); got != "app.R" {
		t.Errorf("with both = %q", got)
	}
}

func TestComputeFileChecksums(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	files := computeFileChecksums(dir)
	if _, ok := files["app.R"]; !ok {
		t.Error("expected app.R in checksums")
	}
	if files["app.R"].Checksum == "" {
		t.Error("expected non-empty checksum")
	}
}
