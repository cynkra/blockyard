package pkgstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateStagingDir(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)

	dir, err := s.CreateStagingDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("staging dir should exist: %v", err)
	}
	// Should be under store root.
	expected := filepath.Join(root, ".staging")
	if !strings.HasPrefix(dir, expected) {
		t.Errorf("dir = %q, expected under %q", dir, expected)
	}
}

func TestCleanupStagingDir(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)

	dir, err := s.CreateStagingDir()
	if err != nil {
		t.Fatal(err)
	}
	// Write a file to make sure cleanup removes contents too.
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hi"), 0o644)

	if err := s.CleanupStagingDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("staging dir should not exist after cleanup")
	}
}
