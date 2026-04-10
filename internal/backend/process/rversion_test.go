package process

import (
	"os"
	"path/filepath"
	"testing"
)

// setupRigFixture creates a fake /opt/R/<version>/bin/R tree under
// dir and temporarily overrides rigBase so ResolveRBinary and
// InstalledRVersions operate against the fixture.
func setupRigFixture(t *testing.T, versions ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, v := range versions {
		binDir := filepath.Join(dir, v, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binDir, "R"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestResolveRBinary_ExactMatch(t *testing.T) {
	dir := setupRigFixture(t, "4.5.0", "4.4.3")
	// Patch rigBase for this test.
	orig := rigBase
	defer func() { setRigBase(orig) }()
	setRigBase(dir)

	path, fell := ResolveRBinary("4.5.0", "/usr/bin/R")
	if fell {
		t.Error("expected exact match, got fallback")
	}
	want := filepath.Join(dir, "4.5.0", "bin", "R")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestResolveRBinary_MinorMatch(t *testing.T) {
	dir := setupRigFixture(t, "4.5.1")
	orig := rigBase
	defer func() { setRigBase(orig) }()
	setRigBase(dir)

	// Request 4.5.0 but only 4.5.1 is installed → minor match.
	path, fell := ResolveRBinary("4.5.0", "/usr/bin/R")
	if fell {
		t.Error("expected minor match, got fallback")
	}
	want := filepath.Join(dir, "4.5.1", "bin", "R")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestResolveRBinary_MinorMatchPicksHighest(t *testing.T) {
	dir := setupRigFixture(t, "4.5.0", "4.5.1", "4.5.2")
	orig := rigBase
	defer func() { setRigBase(orig) }()
	setRigBase(dir)

	// Request 4.5.0 with multiple 4.5.x available → highest patch.
	path, fell := ResolveRBinary("4.5.0", "/usr/bin/R")
	if fell {
		t.Error("expected minor match, got fallback")
	}
	// Exact match takes priority over minor match.
	want := filepath.Join(dir, "4.5.0", "bin", "R")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestResolveRBinary_Fallback(t *testing.T) {
	dir := setupRigFixture(t, "4.4.3")
	orig := rigBase
	defer func() { setRigBase(orig) }()
	setRigBase(dir)

	// Request 4.5.0 but nothing in 4.5.x installed.
	path, fell := ResolveRBinary("4.5.0", "/usr/bin/R")
	if !fell {
		t.Error("expected fallback")
	}
	if path != "/usr/bin/R" {
		t.Errorf("path = %q, want fallback", path)
	}
}

func TestResolveRBinary_EmptyVersion(t *testing.T) {
	path, fell := ResolveRBinary("", "/usr/bin/R")
	if fell {
		t.Error("empty version should not be a fallback")
	}
	if path != "/usr/bin/R" {
		t.Errorf("path = %q, want /usr/bin/R", path)
	}
}

func TestInstalledRVersions(t *testing.T) {
	dir := setupRigFixture(t, "4.3.2", "4.5.0", "4.4.3")
	orig := rigBase
	defer func() { setRigBase(orig) }()
	setRigBase(dir)

	versions := InstalledRVersions()
	if len(versions) != 3 {
		t.Fatalf("expected 3 versions, got %v", versions)
	}
	// Should be sorted.
	if versions[0] != "4.3.2" || versions[1] != "4.4.3" || versions[2] != "4.5.0" {
		t.Errorf("versions = %v", versions)
	}
}

func TestInstalledRVersions_Empty(t *testing.T) {
	dir := t.TempDir() // empty
	orig := rigBase
	defer func() { setRigBase(orig) }()
	setRigBase(dir)

	versions := InstalledRVersions()
	if len(versions) != 0 {
		t.Errorf("expected empty, got %v", versions)
	}
}
