package integration

import (
	"os"
	"path/filepath"
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
