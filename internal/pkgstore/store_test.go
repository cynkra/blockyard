package pkgstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRootAndPlatform(t *testing.T) {
	s := NewStore("/data/.pkg-store")
	if s.Root() != "/data/.pkg-store" {
		t.Errorf("Root() = %q", s.Root())
	}
	if s.Platform() != "" {
		t.Errorf("Platform() = %q before SetPlatform", s.Platform())
	}
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")
	if s.Platform() != "4.5-x86_64-pc-linux-gnu" {
		t.Errorf("Platform() = %q", s.Platform())
	}
}

func TestStoreSourceDir(t *testing.T) {
	s := NewStore("/data/.pkg-store")
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	got := s.SourceDir("shiny", "abc123")
	want := "/data/.pkg-store/4.5-x86_64-pc-linux-gnu/shiny/abc123"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStoreHas(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	if s.Has("shiny", "abc123", "cfg456") {
		t.Error("expected false for empty store")
	}

	// Create the directory.
	p := s.Path("shiny", "abc123", "cfg456")
	os.MkdirAll(p, 0o755)

	if !s.Has("shiny", "abc123", "cfg456") {
		t.Error("expected true after creating directory")
	}
}

func TestStoreIngest(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// Create source directory.
	srcDir := filepath.Join(root, ".builds", "test-build", "shiny")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "DESCRIPTION"), []byte("Package: shiny\n"), 0o644)

	err := s.Ingest("shiny", "abc123", "cfg456", srcDir)
	if err != nil {
		t.Fatal(err)
	}

	// Package should be in the store.
	if !s.Has("shiny", "abc123", "cfg456") {
		t.Error("package not in store after ingest")
	}

	// Source directory should no longer exist (renamed).
	if dirExists(srcDir) {
		t.Error("source directory still exists after ingest")
	}

	// DESCRIPTION should be at the store path.
	desc := filepath.Join(s.Path("shiny", "abc123", "cfg456"), "DESCRIPTION")
	if _, err := os.Stat(desc); err != nil {
		t.Error("DESCRIPTION not found in store")
	}
}

func TestStoreIngestIdempotent(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// Pre-create the store entry.
	os.MkdirAll(s.Path("shiny", "abc123", "cfg456"), 0o755)

	// Create a different source directory.
	srcDir := filepath.Join(root, ".builds", "test", "shiny")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "marker"), []byte("new"), 0o644)

	// Ingest should be a no-op.
	err := s.Ingest("shiny", "abc123", "cfg456", srcDir)
	if err != nil {
		t.Fatal(err)
	}

	// Source should still exist (not renamed since dest already exists).
	if !dirExists(srcDir) {
		t.Error("source directory was removed even though dest already existed")
	}

	// Store entry should not contain the marker.
	marker := filepath.Join(s.Path("shiny", "abc123", "cfg456"), "marker")
	if _, err := os.Stat(marker); err == nil {
		t.Error("marker found in store — idempotent check failed")
	}
}

func TestStorePath(t *testing.T) {
	s := NewStore("/data/.pkg-store")
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	got := s.Path("shiny", "abc123", "cfg456")
	want := "/data/.pkg-store/4.5-x86_64-pc-linux-gnu/shiny/abc123/cfg456"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStoreConfigsPath(t *testing.T) {
	s := NewStore("/data/.pkg-store")
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	got := s.ConfigsPath("shiny", "abc123")
	want := "/data/.pkg-store/4.5-x86_64-pc-linux-gnu/shiny/abc123/configs.json"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStoreConfigMetaPath(t *testing.T) {
	s := NewStore("/data/.pkg-store")
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	got := s.ConfigMetaPath("shiny", "abc123", "cfg456")
	want := "/data/.pkg-store/4.5-x86_64-pc-linux-gnu/shiny/abc123/cfg456.json"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStoreTouch(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// Create config sidecar.
	metaPath := s.ConfigMetaPath("shiny", "abc123", "cfg456")
	os.MkdirAll(filepath.Dir(metaPath), 0o755)
	os.WriteFile(metaPath, []byte(`{"created_at":"2020-01-01T00:00:00Z"}`), 0o644)

	before, _ := os.Stat(metaPath)
	time.Sleep(10 * time.Millisecond) // ensure mtime differs
	s.Touch("shiny", "abc123", "cfg456")

	after, _ := os.Stat(metaPath)
	if !after.ModTime().After(before.ModTime()) {
		t.Error("touch did not update mtime")
	}
}
