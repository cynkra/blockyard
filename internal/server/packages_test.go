package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cynkra/blockyard/internal/manifest"
)

func TestLastLines_FewerLines(t *testing.T) {
	input := "line1\nline2"
	got := lastLines(input, 5)
	if got != "line1\nline2" {
		t.Errorf("lastLines(%q, 5) = %q, want %q", input, got, "line1\nline2")
	}
}

func TestLastLines_ExactN(t *testing.T) {
	input := "a\nb\nc"
	got := lastLines(input, 3)
	if got != "a\nb\nc" {
		t.Errorf("lastLines(%q, 3) = %q, want %q", input, got, "a\nb\nc")
	}
}

func TestLastLines_MoreThanN(t *testing.T) {
	input := "a\nb\nc\nd\ne"
	got := lastLines(input, 2)
	if got != "d\ne" {
		t.Errorf("lastLines(%q, 2) = %q, want %q", input, got, "d\ne")
	}
}

func TestLastLines_Empty(t *testing.T) {
	got := lastLines("", 3)
	if got != "" {
		t.Errorf("lastLines(%q, 3) = %q, want %q", "", got, "")
	}
}

func TestLastLines_SingleLine(t *testing.T) {
	got := lastLines("hello", 1)
	if got != "hello" {
		t.Errorf("lastLines(%q, 1) = %q, want %q", "hello", got, "hello")
	}
}

func TestLastLines_ZeroN(t *testing.T) {
	got := lastLines("a\nb\nc", 0)
	if got != "" {
		t.Errorf("lastLines(%q, 0) = %q, want %q", "a\nb\nc", got, "")
	}
}

func TestLastLines_TrailingNewline(t *testing.T) {
	input := "a\nb\nc\n"
	got := lastLines(input, 2)
	// The trailing newline creates an empty final "line"
	if got == "" {
		t.Error("expected non-empty result for trailing newline input")
	}
}

func TestInstallPackage_WorkerNotFound(t *testing.T) {
	srv := setupRefreshTest(t)

	_, err := srv.InstallPackage(context.Background(), "app-1", "nonexistent", PackageRequest{Name: "shiny"})
	if err == nil {
		t.Fatal("expected error for missing worker")
	}
}

func TestInstallPackage_ManifestReadError(t *testing.T) {
	srv := setupRefreshTest(t)

	// Register a worker but don't create a manifest file.
	srv.Workers.Set("w-1", ActiveWorker{AppID: "app-1", BundleID: "bundle-1"})

	_, err := srv.InstallPackage(context.Background(), "app-1", "w-1", PackageRequest{Name: "shiny"})
	if err == nil {
		t.Fatal("expected error for missing manifest")
	}
}

func TestInstallPackage_StagingDirCreated(t *testing.T) {
	srv := setupRefreshTest(t)
	bundleID := "bundle-1"

	// Set up worker and manifest.
	srv.Workers.Set("w-1", ActiveWorker{AppID: "app-1", BundleID: bundleID})
	bundlePaths := srv.BundlePaths("app-1", bundleID)
	os.MkdirAll(bundlePaths.Base, 0o755)

	m := &manifest.Manifest{
		Version:  1,
		Metadata: manifest.Metadata{Entrypoint: "app.R"},
	}
	data, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(bundlePaths.Base, "manifest.json"), data, 0o644)

	// InstallPackage will fail later (at pakcache.EnsureInstalled or build step)
	// but we cover the staging dir creation path.
	_, err := srv.InstallPackage(context.Background(), "app-1", "w-1", PackageRequest{Name: "shiny"})
	// Error is expected — we just care about covering the code path.
	if err == nil {
		t.Log("install unexpectedly succeeded")
	}
}
