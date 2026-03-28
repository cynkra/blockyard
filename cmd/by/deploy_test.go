package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cynkra/blockyard/internal/manifest"
)

func TestPrepareManifest_CaseManifest(t *testing.T) {
	dir := t.TempDir()
	m := &manifest.Manifest{
		Version:  1,
		Metadata: manifest.Metadata{AppMode: "shiny", Entrypoint: "app.R"},
		Files:    map[string]manifest.FileInfo{"app.R": {Checksum: "abc"}},
	}
	m.Write(filepath.Join(dir, "manifest.json"))
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("# app"), 0o644)

	det := &detectResult{InputCase: caseManifest, Mode: "shiny", Entrypoint: "app.R"}
	result, err := prepareManifest(dir, det, "")
	if err != nil {
		t.Fatalf("prepareManifest: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}
	if result.Metadata.Entrypoint != "app.R" {
		t.Errorf("entrypoint: got %q", result.Metadata.Entrypoint)
	}
}

func TestPrepareManifest_CaseRenvLock(t *testing.T) {
	dir := t.TempDir()
	lockJSON := `{
		"R": {"Version": "4.3.0", "Repositories": [{"Name": "CRAN", "URL": "https://cran.r-project.org"}]},
		"Packages": {
			"shiny": {"Package": "shiny", "Version": "1.9.1", "Source": "Repository"}
		}
	}`
	os.WriteFile(filepath.Join(dir, "renv.lock"), []byte(lockJSON), 0o644)
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("library(shiny)"), 0o644)

	det := &detectResult{InputCase: caseRenvLock, Mode: "shiny", Entrypoint: "app.R"}
	result, err := prepareManifest(dir, det, "")
	if err != nil {
		t.Fatalf("prepareManifest: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}
	if !result.IsPinned() {
		t.Error("expected pinned manifest")
	}
	if _, ok := result.Packages["shiny"]; !ok {
		t.Error("expected shiny package in manifest")
	}
}

func TestPrepareManifest_CaseDescription(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "DESCRIPTION"), []byte("Imports: shiny\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("library(shiny)"), 0o644)

	det := &detectResult{InputCase: caseDescription, Mode: "shiny", Entrypoint: "app.R"}
	result, err := prepareManifest(dir, det, "")
	if err != nil {
		t.Fatalf("prepareManifest: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}
	if result.IsPinned() {
		t.Error("expected unpinned manifest")
	}
	if result.Description["Imports"] != "shiny" {
		t.Errorf("imports: got %q", result.Description["Imports"])
	}
}

func TestPrepareManifest_CaseBareScripts(t *testing.T) {
	dir := t.TempDir()

	det := &detectResult{InputCase: caseBareScripts, Mode: "shiny", Entrypoint: "app.R"}
	result, err := prepareManifest(dir, det, "")
	if err != nil {
		t.Fatalf("prepareManifest: %v", err)
	}
	if result != nil {
		t.Error("expected nil manifest for bare scripts")
	}
}

func TestCreateArchive(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("library(shiny)"), 0o644)
	os.MkdirAll(filepath.Join(dir, "R"), 0o755)
	os.WriteFile(filepath.Join(dir, "R", "helpers.R"), []byte("f <- function() {}"), 0o644)
	// Hidden file should be skipped.
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("secret"), 0o644)

	archive, err := createArchive(dir)
	if err != nil {
		t.Fatalf("createArchive: %v", err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()

	// Read back and verify contents.
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)

	found := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		found[hdr.Name] = true
	}

	if !found["app.R"] {
		t.Error("expected app.R in archive")
	}
	if !found["R/helpers.R"] {
		t.Error("expected R/helpers.R in archive")
	}
	if found[".hidden"] {
		t.Error("hidden file should not be in archive")
	}
}

func TestParseReposFlag(t *testing.T) {
	repos := parseReposFlag("")
	if len(repos) != 1 || repos[0].URL != defaultRepoURL {
		t.Errorf("empty flag should return default repos, got %v", repos)
	}

	repos = parseReposFlag("https://cran.r-project.org,https://bioc.example.com")
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(repos))
	}
	if repos[0].URL != "https://cran.r-project.org" {
		t.Errorf("first repo URL: got %q", repos[0].URL)
	}
	if repos[1].URL != "https://bioc.example.com" {
		t.Errorf("second repo URL: got %q", repos[1].URL)
	}
}

func TestParseReposFlag_WhitespaceOnly(t *testing.T) {
	repos := parseReposFlag("  ,  ,  ")
	if len(repos) != 1 || repos[0].URL != defaultRepoURL {
		t.Errorf("whitespace-only entries should fall back to defaults, got %v", repos)
	}
}

func TestCreateArchive_SkipsHiddenDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.R"), []byte("library(shiny)"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main"), 0o644)

	archive, err := createArchive(dir)
	if err != nil {
		t.Fatalf("createArchive: %v", err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()

	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		if hdr.Name == ".git" || hdr.Name == ".git/HEAD" || hdr.Name == ".git/objects" {
			t.Errorf("hidden directory contents should be skipped, found %q", hdr.Name)
		}
	}
}

func TestCreateArchive_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	archive, err := createArchive(dir)
	if err != nil {
		t.Fatalf("createArchive: %v", err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()

	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)

	count := 0
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		count++
	}
	if count != 0 {
		t.Errorf("expected empty archive, got %d entries", count)
	}
}

func TestPrepareManifest_UnknownCase(t *testing.T) {
	det := &detectResult{InputCase: inputCase(99), Mode: "shiny", Entrypoint: "app.R"}
	_, err := prepareManifest(t.TempDir(), det, "")
	if err == nil {
		t.Fatal("expected error for unknown input case")
	}
	if err.Error() != "unknown input case" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDirExists(t *testing.T) {
	dir := t.TempDir()
	if !dirExists(dir) {
		t.Error("expected true for existing directory")
	}
	if dirExists(filepath.Join(dir, "nonexistent")) {
		t.Error("expected false for nonexistent path")
	}
	// File is not a directory.
	f := filepath.Join(dir, "file.txt")
	os.WriteFile(f, []byte("hi"), 0o644)
	if dirExists(f) {
		t.Error("expected false for regular file")
	}
}
