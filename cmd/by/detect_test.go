package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectApp_ManifestJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":1,"metadata":{"appmode":"shiny","entrypoint":"app.R"},"files":{}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)
	// Also create renv.lock to test warning.
	os.WriteFile(filepath.Join(dir, "renv.lock"), []byte("{}"), 0o644)

	det, warnings := detectApp(dir, false)
	if det.InputCase != caseManifest {
		t.Errorf("expected caseManifest, got %v", det.InputCase)
	}
	if len(warnings) == 0 {
		t.Error("expected warning about ignoring renv.lock")
	}
}

func TestDetectApp_RenvLock(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "renv.lock"), []byte(`{"R":{"Version":"4.3.0","Repositories":[]},"Packages":{}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	det, _ := detectApp(dir, false)
	if det.InputCase != caseRenvLock {
		t.Errorf("expected caseRenvLock, got %v", det.InputCase)
	}
	if det.Entrypoint != "app.R" {
		t.Errorf("expected entrypoint app.R, got %s", det.Entrypoint)
	}
}

func TestDetectApp_Description(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "DESCRIPTION"), []byte("Imports: shiny\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	det, _ := detectApp(dir, false)
	if det.InputCase != caseDescription {
		t.Errorf("expected caseDescription, got %v", det.InputCase)
	}
}

func TestDetectApp_BareScripts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	det, _ := detectApp(dir, false)
	if det.InputCase != caseBareScripts {
		t.Errorf("expected caseBareScripts, got %v", det.InputCase)
	}
}

func TestDetectApp_PinFlag(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	det, _ := detectApp(dir, true)
	if det.InputCase != casePinFlag {
		t.Errorf("expected casePinFlag, got %v", det.InputCase)
	}
}

func TestDetectApp_Priority(t *testing.T) {
	// manifest.json > renv.lock > DESCRIPTION.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":1,"metadata":{"appmode":"shiny","entrypoint":"app.R"},"files":{}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "renv.lock"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "DESCRIPTION"), []byte("Imports: shiny\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	det, warnings := detectApp(dir, false)
	if det.InputCase != caseManifest {
		t.Errorf("expected caseManifest, got %v", det.InputCase)
	}
	if len(warnings) != 2 {
		t.Errorf("expected 2 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestDetectEntrypoint_ServerR(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "server.R"), []byte("# server"), 0o644)
	os.WriteFile(filepath.Join(dir, "ui.R"), []byte("# ui"), 0o644)

	ep := detectEntrypoint(dir)
	if ep != "server.R" {
		t.Errorf("expected server.R, got %s", ep)
	}
}

func TestComputeFileChecksums(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("library(shiny)"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "helper.R"), []byte("f <- function() {}"), 0o644)

	files := computeFileChecksums(dir)

	if _, ok := files["app.R"]; !ok {
		t.Error("expected app.R in checksums")
	}
	if files["app.R"].Checksum == "" {
		t.Error("expected non-empty checksum")
	}
	if _, ok := files["sub/helper.R"]; !ok {
		t.Error("expected sub/helper.R in checksums")
	}
}

func TestComputeFileChecksums_SkipsHidden(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("secret"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("x"), 0o644)

	files := computeFileChecksums(dir)
	if _, ok := files[".hidden"]; ok {
		t.Error("should skip hidden files")
	}
	if _, ok := files[".git/config"]; ok {
		t.Error("should skip .git directory")
	}
}

func TestComputeFileChecksums_SkipsRenvArtifacts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)
	os.WriteFile(filepath.Join(dir, "renv.lock"), []byte("{}"), 0o644)
	os.MkdirAll(filepath.Join(dir, "renv", "lib"), 0o755)
	os.WriteFile(filepath.Join(dir, "renv", "lib", "pkg.tar.gz"), []byte("x"), 0o644)

	files := computeFileChecksums(dir)
	if _, ok := files["renv.lock"]; ok {
		t.Error("should skip renv.lock")
	}
	if _, ok := files["renv/lib/pkg.tar.gz"]; ok {
		t.Error("should skip renv/ directory")
	}
}
