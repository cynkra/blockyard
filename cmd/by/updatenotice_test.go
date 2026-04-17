package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUpdateCache_Missing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c := loadUpdateCache()
	if c != nil {
		t.Error("expected nil for missing cache")
	}
}

func TestLoadUpdateCache_Valid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cacheDir := filepath.Join(dir, "by")
	os.MkdirAll(cacheDir, 0o700)
	os.WriteFile(filepath.Join(cacheDir, "update-check.json"),
		[]byte(`{"latest_version":"2.0.0","state":"update_available","checked_at":"2026-01-01T00:00:00Z"}`),
		0o600)

	c := loadUpdateCache()
	if c == nil {
		t.Fatal("expected non-nil cache")
	}
	if c.LatestVersion != "2.0.0" {
		t.Errorf("LatestVersion = %q, want 2.0.0", c.LatestVersion)
	}
	if c.State != "update_available" {
		t.Errorf("State = %q, want update_available", c.State)
	}
}

func TestLoadUpdateCache_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cacheDir := filepath.Join(dir, "by")
	os.MkdirAll(cacheDir, 0o700)
	os.WriteFile(filepath.Join(cacheDir, "update-check.json"),
		[]byte(`not json`), 0o600)

	c := loadUpdateCache()
	if c != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestSaveUpdateCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	saveUpdateCache(&updateCache{
		LatestVersion: "3.0.0",
		State:         "update_available",
	})

	// Verify file was created.
	data, err := os.ReadFile(filepath.Join(dir, "by", "update-check.json"))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty cache file")
	}

	// Verify round-trip.
	c := loadUpdateCache()
	if c == nil {
		t.Fatal("expected non-nil cache after save")
	}
	if c.LatestVersion != "3.0.0" {
		t.Errorf("LatestVersion = %q, want 3.0.0", c.LatestVersion)
	}
}

func TestPrintUpdateNotice(t *testing.T) {
	old := version
	version = "1.0.0"
	defer func() { version = old }()

	// Just ensure it doesn't panic — output goes to stderr.
	printUpdateNotice("2.0.0")
}
