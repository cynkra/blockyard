package buildercache

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFileExistsTrue(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "exists")
	os.WriteFile(path, []byte("data"), 0o644)

	if !fileExists(path) {
		t.Error("expected fileExists to return true for existing file")
	}
}

func TestFileExistsFalse(t *testing.T) {
	if fileExists("/nonexistent/path/file") {
		t.Error("expected fileExists to return false for nonexistent file")
	}
}

func TestFileExistsDir(t *testing.T) {
	tmp := t.TempDir()
	if fileExists(tmp) {
		t.Error("expected fileExists to return false for directory")
	}
}

func TestFindModuleRoot(t *testing.T) {
	root, err := findModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Errorf("go.mod not found at module root %s", root)
	}
}

func TestEnsureCachedHit(t *testing.T) {
	tmp := t.TempDir()
	version := "test"
	name := "by-builder-" + version + "-linux-" + runtime.GOARCH
	binPath := filepath.Join(tmp, name)

	// Pre-populate cache.
	os.WriteFile(binPath, []byte("#!/bin/sh"), 0o755)

	got, err := EnsureCached(tmp, version)
	if err != nil {
		t.Fatal(err)
	}
	if got != binPath {
		t.Errorf("expected %s, got %s", binPath, got)
	}
}

func TestCopyBinary(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	src := filepath.Join(srcDir, "src-bin")
	dst := filepath.Join(dstDir, "dst-bin")

	content := []byte("#!/bin/sh\necho hello")
	os.WriteFile(src, content, 0o755)

	if err := copyBinary(src, dst); err != nil {
		t.Fatalf("copyBinary: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch: got %q", got)
	}

	info, _ := os.Stat(dst)
	if info.Mode().Perm() != 0o755 {
		t.Errorf("permissions: got %o, want 755", info.Mode().Perm())
	}

	// Temp file should be cleaned up.
	if fileExists(dst + ".tmp") {
		t.Error(".tmp file should not remain")
	}
}

func TestCopyBinary_SourceMissing(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dst")
	if err := copyBinary("/nonexistent/file", dst); err == nil {
		t.Error("expected error for missing source")
	}
}

func TestEnsureCachedBuildFromSource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build test in short mode")
	}

	tmp := t.TempDir()
	got, err := EnsureCached(tmp, "dev")
	if err != nil {
		t.Fatalf("EnsureCached: %v", err)
	}
	if !fileExists(got) {
		t.Errorf("binary not found at %s", got)
	}
}
