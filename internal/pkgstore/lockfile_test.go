package pkgstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadLockfile(t *testing.T) {
	data := `{
  "lockfile_version": 1,
  "r_version": "R version 4.5.2 (2025-10-31)",
  "os": "Ubuntu 24.04.2 LTS",
  "platform": "x86_64-pc-linux-gnu",
  "packages": [
    {
      "package": "shiny",
      "version": "1.9.1",
      "type": "standard",
      "needscompilation": false,
      "metadata": {"RemoteType": "standard", "RemoteSha": "1.9.1"},
      "sha256": "abc123",
      "platform": "x86_64-pc-linux-gnu",
      "rversion": "4.5"
    }
  ]
}`
	dir := t.TempDir()
	path := filepath.Join(dir, "pak.lock")
	os.WriteFile(path, []byte(data), 0o644)

	lf, err := ReadLockfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lf.Packages) != 1 {
		t.Fatalf("expected 1 package, got %d", len(lf.Packages))
	}
	if lf.Packages[0].Package != "shiny" {
		t.Errorf("package = %q", lf.Packages[0].Package)
	}
}

func TestReadLockfile_UnsupportedVersion(t *testing.T) {
	data := `{"lockfile_version": 2, "packages": [{"package": "x", "version": "1", "type": "standard", "sha256": "a", "platform": "x", "rversion": "4", "metadata": {"RemoteType": "standard"}}]}`
	dir := t.TempDir()
	path := filepath.Join(dir, "pak.lock")
	os.WriteFile(path, []byte(data), 0o644)

	_, err := ReadLockfile(path)
	if err == nil {
		t.Error("expected error for lockfile_version 2")
	}
}

func TestReadLockfile_EmptyPackages(t *testing.T) {
	data := `{"lockfile_version": 1, "packages": []}`
	dir := t.TempDir()
	path := filepath.Join(dir, "pak.lock")
	os.WriteFile(path, []byte(data), 0o644)

	_, err := ReadLockfile(path)
	if err == nil {
		t.Error("expected error for empty packages")
	}
}

func TestLockfileEntryValidate_Standard(t *testing.T) {
	e := LockfileEntry{
		Package:  "shiny",
		Version:  "1.9.1",
		SHA256:   "abc123",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "standard"},
	}
	if err := e.Validate(); err != nil {
		t.Errorf("valid standard entry: %v", err)
	}
}

func TestLockfileEntryValidate_StandardMissingSha(t *testing.T) {
	e := LockfileEntry{
		Package:  "shiny",
		Version:  "1.9.1",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "standard"},
	}
	if err := e.Validate(); err == nil {
		t.Error("expected error for standard without sha256")
	}
}

func TestLockfileEntryValidate_GitHub(t *testing.T) {
	e := LockfileEntry{
		Package:  "pkg",
		Version:  "1.0",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{
			RemoteType: "github",
			RemoteSha:  "deadbeef",
		},
	}
	if err := e.Validate(); err != nil {
		t.Errorf("valid github entry: %v", err)
	}
}

func TestLockfileEntryValidate_MissingType(t *testing.T) {
	e := LockfileEntry{
		Package:  "pkg",
		Version:  "1.0",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
	}
	if err := e.Validate(); err == nil {
		t.Error("expected error for missing type")
	}
}

func TestLockfileEntryValidate_MissingRversion(t *testing.T) {
	e := LockfileEntry{
		Package: "pkg",
		Version: "1.0",
		Type:    "standard",
		SHA256:  "abc",
	}
	if err := e.Validate(); err == nil {
		t.Error("expected error for missing rversion")
	}
}

func TestLockfileEntryValidate_UnsupportedType(t *testing.T) {
	e := LockfileEntry{
		Package:  "pkg",
		Version:  "1.0",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "url"},
	}
	err := e.Validate()
	if err == nil {
		t.Error("expected error for url type")
	}
}

func TestReadLockfile_FileNotFound(t *testing.T) {
	_, err := ReadLockfile("/nonexistent/pak.lock")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestReadLockfile_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pak.lock")
	os.WriteFile(path, []byte(`{not json`), 0o644)

	_, err := ReadLockfile(path)
	if err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestLockfileValidate_EntryError(t *testing.T) {
	// Lockfile is valid at the top level but has an invalid entry.
	lf := Lockfile{
		LockfileVersion: 1,
		Packages: []LockfileEntry{
			{Package: "", Version: "1.0"}, // missing package name
		},
	}
	if err := lf.Validate(); err == nil {
		t.Error("expected error for invalid entry")
	}
}

func TestLockfileEntryValidate_MissingPackage(t *testing.T) {
	e := LockfileEntry{Version: "1.0", RVersion: "4.5", Platform: "x", Metadata: LockfileMetadata{RemoteType: "standard"}, SHA256: "a"}
	if err := e.Validate(); err == nil {
		t.Error("expected error for missing package")
	}
}

func TestLockfileEntryValidate_MissingVersion(t *testing.T) {
	e := LockfileEntry{Package: "pkg", RVersion: "4.5", Platform: "x", Metadata: LockfileMetadata{RemoteType: "standard"}, SHA256: "a"}
	if err := e.Validate(); err == nil {
		t.Error("expected error for missing version")
	}
}

func TestLockfileEntryValidate_MissingPlatform(t *testing.T) {
	e := LockfileEntry{Package: "pkg", Version: "1.0", RVersion: "4.5", Metadata: LockfileMetadata{RemoteType: "standard"}, SHA256: "a"}
	if err := e.Validate(); err == nil {
		t.Error("expected error for missing platform")
	}
}

func TestLockfileEntryValidate_GitHubMissingRemoteSha(t *testing.T) {
	e := LockfileEntry{
		Package:  "pkg",
		Version:  "1.0",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "github"},
		// RemoteSha is empty
	}
	if err := e.Validate(); err == nil {
		t.Error("expected error for github without RemoteSha")
	}
}

func TestLockfileEntryValidate_FallbackToType(t *testing.T) {
	// When metadata.RemoteType is empty, falls back to top-level Type.
	e := LockfileEntry{
		Package:  "pkg",
		Version:  "1.0",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Type:     "standard",
		SHA256:   "abc",
	}
	if err := e.Validate(); err != nil {
		t.Errorf("should fall back to top-level type: %v", err)
	}
}

func TestPlatformFromLockfile(t *testing.T) {
	lf := &Lockfile{
		Packages: []LockfileEntry{
			{
				Package:  "shiny",
				RVersion: "4.5",
				Platform: "x86_64-pc-linux-gnu",
			},
		},
	}
	got := PlatformFromLockfile(lf)
	if got != "4.5-x86_64-pc-linux-gnu" {
		t.Errorf("got %q", got)
	}
}

func TestPlatformFromLockfile_Empty(t *testing.T) {
	lf := &Lockfile{}
	got := PlatformFromLockfile(lf)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}
