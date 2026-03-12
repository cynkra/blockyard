package bundle

import (
	"bytes"
	"os"
	"testing"

	"github.com/cynkra/blockyard/internal/testutil"
)

func TestWriteAndUnpackArchive(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeBundle(t)
	if err := WriteArchive(paths, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(paths.Archive); err != nil {
		t.Fatalf("archive not found: %v", err)
	}

	if err := UnpackArchive(paths); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(paths.Unpacked + "/app.R"); err != nil {
		t.Fatal("app.R not found in unpacked dir")
	}
}

func TestDeleteFiles(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeBundle(t)
	WriteArchive(paths, bytes.NewReader(data))
	UnpackArchive(paths)
	CreateLibraryDir(paths)

	DeleteFiles(paths)

	for _, p := range []string{paths.Archive, paths.Unpacked, paths.Library} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted", p)
		}
	}
}

func TestPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	paths := NewBundlePaths(tmp, "app-1", "bundle-1")

	data := testutil.MakeTraversalBundle(t)
	if err := WriteArchive(paths, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	err := UnpackArchive(paths)
	if err == nil {
		t.Fatal("expected error on path traversal")
	}
}
