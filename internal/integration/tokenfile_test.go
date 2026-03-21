package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".vault-token")

	if err := WriteTokenFile(path, "hvs.test-token"); err != nil {
		t.Fatal(err)
	}

	got, err := ReadTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hvs.test-token" {
		t.Errorf("got %q, want hvs.test-token", got)
	}
}

func TestTokenFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".vault-token")

	if err := WriteTokenFile(path, "secret"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
}

func TestTokenFileOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".vault-token")

	WriteTokenFile(path, "old-token")
	WriteTokenFile(path, "new-token")

	got, err := ReadTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "new-token" {
		t.Errorf("got %q, want new-token", got)
	}
}

func TestReadTokenFileMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist")

	got, err := ReadTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestReadTokenFileTrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".vault-token")
	os.WriteFile(path, []byte("  hvs.token  \n"), 0o600)

	got, err := ReadTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hvs.token" {
		t.Errorf("got %q, want hvs.token", got)
	}
}

func TestReadTokenFilePermissionError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".vault-token")
	os.WriteFile(path, []byte("secret"), 0o600)
	// Make directory unreadable so the file can't be read.
	os.Chmod(dir, 0o000)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	_, err := ReadTokenFile(path)
	if err == nil {
		t.Fatal("expected error for permission-denied read")
	}
	if !strings.Contains(err.Error(), "read token file") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWriteTokenFileBadDirectory(t *testing.T) {
	// Writing to a non-existent directory should fail at CreateTemp.
	path := filepath.Join(t.TempDir(), "no-such-dir", ".vault-token")
	err := WriteTokenFile(path, "token")
	if err == nil {
		t.Fatal("expected error for non-existent directory")
	}
	if !strings.Contains(err.Error(), "create temp") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWriteTokenFileRenameFailure(t *testing.T) {
	dir := t.TempDir()
	// Create a subdirectory as the target path — rename onto a directory fails.
	path := filepath.Join(dir, "subdir")
	os.Mkdir(path, 0o755)
	// Place a file inside so the directory is non-empty and rename fails.
	os.WriteFile(filepath.Join(path, "blocker"), []byte("x"), 0o644)

	err := WriteTokenFile(path, "token")
	if err == nil {
		t.Fatal("expected error for rename onto directory")
	}
}
