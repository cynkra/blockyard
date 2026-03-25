package pkgstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAssembleLibrary(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// Create a store entry.
	storePath := s.Path("shiny", "src1", "cfg1")
	os.MkdirAll(storePath, 0o755)
	os.WriteFile(filepath.Join(storePath, "DESCRIPTION"), []byte("Package: shiny\n"), 0o644)

	// Create config sidecar for Touch.
	metaPath := s.ConfigMetaPath("shiny", "src1", "cfg1")
	os.WriteFile(metaPath, []byte(`{"created_at":"2020-01-01T00:00:00Z"}`), 0o644)

	libDir := filepath.Join(root, ".workers", "w1")
	manifest := map[string]string{
		"shiny": "src1/cfg1",
	}

	missing, err := s.AssembleLibrary(libDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("unexpected missing: %v", missing)
	}

	// Check that the package was hard-linked.
	desc := filepath.Join(libDir, "shiny", "DESCRIPTION")
	if _, err := os.Stat(desc); err != nil {
		t.Error("DESCRIPTION not found in assembled library")
	}

	// Check .packages.json was written.
	pm, err := ReadPackageManifest(libDir)
	if err != nil {
		t.Fatal(err)
	}
	if pm["shiny"] != "src1/cfg1" {
		t.Errorf("manifest entry: %q", pm["shiny"])
	}
}

func TestAssembleLibraryMissing(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// Only create one of two entries.
	storePath := s.Path("shiny", "src1", "cfg1")
	os.MkdirAll(storePath, 0o755)
	os.WriteFile(filepath.Join(storePath, "DESCRIPTION"), []byte("Package: shiny\n"), 0o644)
	metaPath := s.ConfigMetaPath("shiny", "src1", "cfg1")
	os.WriteFile(metaPath, []byte(`{}`), 0o644)

	libDir := filepath.Join(root, ".workers", "w2")
	manifest := map[string]string{
		"shiny":   "src1/cfg1",
		"ggplot2": "src2/cfg2", // not in store
	}

	missing, err := s.AssembleLibrary(libDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != "ggplot2" {
		t.Errorf("missing = %v", missing)
	}

	// shiny should still be linked.
	if _, err := os.Stat(filepath.Join(libDir, "shiny", "DESCRIPTION")); err != nil {
		t.Error("shiny should still be assembled")
	}
}

func TestAssembleLibraryEmpty(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	libDir := filepath.Join(root, ".workers", "w3")
	missing, err := s.AssembleLibrary(libDir, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("unexpected missing: %v", missing)
	}

	// Dir should exist.
	if _, err := os.Stat(libDir); err != nil {
		t.Error("lib dir not created")
	}
}

func TestAssembleLibrary_BadStoreRef(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	libDir := filepath.Join(root, ".workers", "w4")
	manifest := map[string]string{
		"shiny": "malformed-no-slash",
	}

	_, err := s.AssembleLibrary(libDir, manifest)
	if err == nil {
		t.Error("expected error for malformed store ref")
	}
}

func TestSplitStoreRef(t *testing.T) {
	src, cfg, err := SplitStoreRef("abc123/def456")
	if err != nil {
		t.Fatal(err)
	}
	if src != "abc123" || cfg != "def456" {
		t.Errorf("got %q, %q", src, cfg)
	}
}

func TestSplitStoreRef_Malformed(t *testing.T) {
	for _, ref := range []string{"", "noslash", "/empty", "empty/"} {
		_, _, err := SplitStoreRef(ref)
		if err == nil {
			t.Errorf("expected error for %q", ref)
		}
	}
}

func TestWorkerLibDir(t *testing.T) {
	s := NewStore("/data/.pkg-store")
	got := s.WorkerLibDir("worker-123")
	want := "/data/.pkg-store/.workers/worker-123"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCleanupWorkerLib(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)

	wDir := s.WorkerLibDir("w1")
	os.MkdirAll(wDir, 0o755)
	os.WriteFile(filepath.Join(wDir, "test"), []byte("x"), 0o644)

	if err := s.CleanupWorkerLib("w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wDir); !os.IsNotExist(err) {
		t.Error("worker lib dir still exists")
	}
}

func TestCleanupWorkerLibNonexistent(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)

	if err := s.CleanupWorkerLib("nonexistent"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
