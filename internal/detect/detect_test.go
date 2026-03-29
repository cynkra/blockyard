package detect

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApp_ManifestJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":1,"metadata":{"appmode":"shiny","entrypoint":"app.R"},"files":{}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)
	// Also create renv.lock to test warning.
	os.WriteFile(filepath.Join(dir, "renv.lock"), []byte("{}"), 0o644)

	det, warnings := App(dir, false)
	if det.InputCase != CaseManifest {
		t.Errorf("expected CaseManifest, got %v", det.InputCase)
	}
	if len(warnings) == 0 {
		t.Error("expected warning about ignoring renv.lock")
	}
}

func TestApp_RenvLock(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "renv.lock"), []byte(`{"R":{"Version":"4.3.0","Repositories":[]},"Packages":{}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	det, _ := App(dir, false)
	if det.InputCase != CaseRenvLock {
		t.Errorf("expected CaseRenvLock, got %v", det.InputCase)
	}
	if det.Entrypoint != "app.R" {
		t.Errorf("expected entrypoint app.R, got %s", det.Entrypoint)
	}
}

func TestApp_Description(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "DESCRIPTION"), []byte("Imports: shiny\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	det, _ := App(dir, false)
	if det.InputCase != CaseDescription {
		t.Errorf("expected CaseDescription, got %v", det.InputCase)
	}
}

func TestApp_BareScripts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	det, _ := App(dir, false)
	if det.InputCase != CaseBareScripts {
		t.Errorf("expected CaseBareScripts, got %v", det.InputCase)
	}
}

func TestApp_PinFlag(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	det, _ := App(dir, true)
	if det.InputCase != CasePinFlag {
		t.Errorf("expected CasePinFlag, got %v", det.InputCase)
	}
}

func TestApp_Priority(t *testing.T) {
	// manifest.json > renv.lock > DESCRIPTION.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":1,"metadata":{"appmode":"shiny","entrypoint":"app.R"},"files":{}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "renv.lock"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "DESCRIPTION"), []byte("Imports: shiny\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	det, warnings := App(dir, false)
	if det.InputCase != CaseManifest {
		t.Errorf("expected CaseManifest, got %v", det.InputCase)
	}
	if len(warnings) != 2 {
		t.Errorf("expected 2 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestEntrypoint_ServerR(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "server.R"), []byte("# server"), 0o644)
	os.WriteFile(filepath.Join(dir, "ui.R"), []byte("# ui"), 0o644)

	ep := Entrypoint(dir)
	if ep != "server.R" {
		t.Errorf("expected server.R, got %s", ep)
	}
}

func TestFileChecksums(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("library(shiny)"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "helper.R"), []byte("f <- function() {}"), 0o644)

	files := FileChecksums(dir)

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

func TestFileChecksums_SkipsHidden(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("secret"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("x"), 0o644)

	files := FileChecksums(dir)
	if _, ok := files[".hidden"]; ok {
		t.Error("should skip hidden files")
	}
	if _, ok := files[".git/config"]; ok {
		t.Error("should skip .git directory")
	}
}

func TestFileChecksums_SkipsRenvArtifacts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)
	os.WriteFile(filepath.Join(dir, "renv.lock"), []byte("{}"), 0o644)
	os.MkdirAll(filepath.Join(dir, "renv", "lib"), 0o755)
	os.WriteFile(filepath.Join(dir, "renv", "lib", "pkg.tar.gz"), []byte("x"), 0o644)

	files := FileChecksums(dir)
	if _, ok := files["renv.lock"]; ok {
		t.Error("should skip renv.lock")
	}
	if _, ok := files["renv/lib/pkg.tar.gz"]; ok {
		t.Error("should skip renv/ directory")
	}
}

func TestInputCaseString(t *testing.T) {
	tests := []struct {
		c    InputCase
		want string
	}{
		{CaseManifest, "manifest.json"},
		{CaseRenvLock, "renv.lock"},
		{CasePinFlag, "--pin (renv snapshot)"},
		{CaseDescription, "DESCRIPTION"},
		{CaseBareScripts, "bare scripts"},
		{InputCase(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.c.String(); got != tt.want {
			t.Errorf("InputCase(%d).String() = %q, want %q", tt.c, got, tt.want)
		}
	}
}

func TestDefaultRepositories(t *testing.T) {
	repos := DefaultRepositories()
	if len(repos) != 1 {
		t.Fatalf("expected 1 repository, got %d", len(repos))
	}
	if repos[0].Name != "CRAN" {
		t.Errorf("expected name CRAN, got %q", repos[0].Name)
	}
	if repos[0].URL != DefaultRepoURL {
		t.Errorf("expected URL %q, got %q", DefaultRepoURL, repos[0].URL)
	}
}

func TestDirExists(t *testing.T) {
	dir := t.TempDir()
	if !DirExists(dir) {
		t.Error("expected true for existing directory")
	}
	if DirExists(filepath.Join(dir, "nonexistent")) {
		t.Error("expected false for nonexistent path")
	}
	// File is not a directory.
	f := filepath.Join(dir, "file.txt")
	os.WriteFile(f, []byte("hi"), 0o644)
	if DirExists(f) {
		t.Error("expected false for regular file")
	}
}
