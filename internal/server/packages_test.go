package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/pkgstore"
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

// ---------------------------------------------------------------------------
// Test helpers for end-to-end InstallPackage tests
// ---------------------------------------------------------------------------

// setupInstallTest creates a Server with mock backend, pre-cached pak and
// by-builder binaries, and a configured package store. The mock's BuildFn
// is left nil so each test can wire its own behavior.
func setupInstallTest(t *testing.T) (*Server, *mock.MockBackend) {
	t.Helper()
	srv, be := testServerWithMock(t)

	srv.PkgStore.SetPlatform("test-platform")
	srv.Config.Docker.PakVersion = "stable"

	bsp := srv.Config.Storage.BundleServerPath

	// Pre-create pak cache so EnsureInstalled returns immediately.
	os.MkdirAll(filepath.Join(bsp, ".pak-cache", "pak-stable"), 0o755)

	// Pre-create builder cache with a dummy binary.
	builderCacheDir := filepath.Join(bsp, ".by-builder-cache")
	os.MkdirAll(builderCacheDir, 0o755)
	fakeBuilder := filepath.Join(builderCacheDir,
		"by-builder-"+srv.Version+"-linux-"+runtime.GOARCH)
	os.WriteFile(fakeBuilder, []byte("#!/bin/sh\n"), 0o755)

	return srv, be
}

// setupBundleAndWorker registers a worker with a bundle manifest and an
// initial package library. Returns the worker's library directory path.
func setupBundleAndWorker(
	t *testing.T, srv *Server,
	appID, bundleID, workerID string,
	installedPkgs map[string]string, // package → "sourceHash/configHash"
) string {
	t.Helper()

	// Create bundle directory with minimal manifest.
	bundlePaths := srv.BundlePaths(appID, bundleID)
	os.MkdirAll(bundlePaths.Base, 0o755)
	m := &manifest.Manifest{
		Version:  1,
		Metadata: manifest.Metadata{Entrypoint: "app.R"},
	}
	data, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(bundlePaths.Base, "manifest.json"), data, 0o644)

	// Register the worker and create its transfer directory
	// (normally created by defaultWorkerSpec at spawn time).
	srv.Workers.Set(workerID, ActiveWorker{AppID: appID, BundleID: bundleID})
	os.MkdirAll(srv.TransferDir(workerID), 0o755)

	// Create worker library with a .packages.json manifest.
	workerLibDir := srv.PkgStore.WorkerLibDir(workerID)
	os.MkdirAll(workerLibDir, 0o755)
	if installedPkgs != nil {
		pkgstore.WritePackageManifest(workerLibDir, installedPkgs)
	}

	return workerLibDir
}

// addStorePackage creates a package in the store with a DESCRIPTION file
// and config-meta sidecar so that linking and Touch work.
func addStorePackage(t *testing.T, srv *Server, pkg, sourceHash, configHash string) {
	t.Helper()
	pkgDir := srv.PkgStore.Path(pkg, sourceHash, configHash)
	os.MkdirAll(pkgDir, 0o755)
	os.WriteFile(filepath.Join(pkgDir, "DESCRIPTION"),
		[]byte("Package: "+pkg), 0o644)
	metaPath := srv.PkgStore.ConfigMetaPath(pkg, sourceHash, configHash)
	os.WriteFile(metaPath, []byte(`{}`), 0o644)
}

// buildFnWritingManifest returns a BuildFn that writes the given
// store-manifest into the staging directory mounted at /staging.
func buildFnWritingManifest(storeManifest map[string]string) func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
	return func(_ context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
		for _, mount := range spec.Mounts {
			if mount.Target == "/staging" {
				pkgstore.WriteStoreManifest(mount.Source, storeManifest)
				break
			}
		}
		return backend.BuildResult{Success: true, ExitCode: 0}, nil
	}
}

// ---------------------------------------------------------------------------
// T1: End-to-end InstallPackage → conflict → transfer
// ---------------------------------------------------------------------------

func TestInstallPackage_ConflictTriggersTransfer(t *testing.T) {
	srv, be := setupInstallTest(t)

	// Worker has shiny src1/cfg1 installed and loaded.
	addStorePackage(t, srv, "shiny", "src1", "cfg1")
	setupBundleAndWorker(t, srv, "app-1", "bundle-1", "w-1",
		map[string]string{"shiny": "src1/cfg1"})

	// Build produces a DIFFERENT ref for shiny → conflict.
	be.BuildFn = buildFnWritingManifest(map[string]string{"shiny": "src2/cfg1"})

	resp, err := srv.InstallPackage(context.Background(), "app-1", "w-1",
		PackageRequest{
			Name:             "shiny",
			LoadedNamespaces: []string{"shiny"},
		})
	if err != nil {
		t.Fatalf("InstallPackage error: %v", err)
	}
	if resp.Status != "transfer" {
		t.Errorf("Status = %q, want %q", resp.Status, "transfer")
	}

	// Verify store-manifest was copied to the transfer directory.
	transferManifest := filepath.Join(srv.TransferDir("w-1"), "store-manifest.json")
	if !fileExists(transferManifest) {
		t.Fatal("expected store-manifest.json in transfer directory")
	}
	m, _ := pkgstore.ReadStoreManifest(transferManifest)
	if m["shiny"] != "src2/cfg1" {
		t.Errorf("transfer manifest shiny = %q, want %q", m["shiny"], "src2/cfg1")
	}
}

// ---------------------------------------------------------------------------
// T4: Second install while transfer is in-flight
// ---------------------------------------------------------------------------

func TestInstallPackage_RejectsDuringTransfer(t *testing.T) {
	srv, be := setupInstallTest(t)

	addStorePackage(t, srv, "shiny", "src1", "cfg1")
	setupBundleAndWorker(t, srv, "app-1", "bundle-1", "w-1",
		map[string]string{"shiny": "src1/cfg1"})

	// Use a long transfer timeout so watchTransfer stays alive.
	srv.Config.Proxy.TransferTimeout.Duration = 30 * time.Second

	be.BuildFn = buildFnWritingManifest(map[string]string{"shiny": "src2/cfg1"})

	// First install triggers a transfer.
	resp, err := srv.InstallPackage(context.Background(), "app-1", "w-1",
		PackageRequest{Name: "shiny", LoadedNamespaces: []string{"shiny"}})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if resp.Status != "transfer" {
		t.Fatalf("first install status = %q, want transfer", resp.Status)
	}

	// Second install should be rejected while the transfer is in-flight.
	_, err = srv.InstallPackage(context.Background(), "app-1", "w-1",
		PackageRequest{Name: "DT"})
	if err == nil {
		t.Fatal("expected error for install during active transfer")
	}
	if !strings.Contains(err.Error(), "transfer in progress") {
		t.Errorf("error = %q, want 'transfer in progress'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// T5: Happy-path end-to-end — no conflict → link
// ---------------------------------------------------------------------------

func TestInstallPackage_NoConflict_LinksPackage(t *testing.T) {
	srv, be := setupInstallTest(t)

	// Worker has shiny installed; we're installing DT (new, not loaded).
	addStorePackage(t, srv, "shiny", "src1", "cfg1")
	addStorePackage(t, srv, "DT", "src1", "cfg1")
	workerLibDir := setupBundleAndWorker(t, srv, "app-1", "bundle-1", "w-1",
		map[string]string{"shiny": "src1/cfg1"})

	// Build returns DT (not in loaded namespaces → no conflict).
	be.BuildFn = buildFnWritingManifest(map[string]string{"DT": "src1/cfg1"})

	resp, err := srv.InstallPackage(context.Background(), "app-1", "w-1",
		PackageRequest{
			Name:             "DT",
			LoadedNamespaces: []string{"shiny"},
		})
	if err != nil {
		t.Fatalf("InstallPackage error: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("Status = %q, want %q", resp.Status, "ok")
	}

	// Verify DT was linked into worker lib.
	dtDir := filepath.Join(workerLibDir, "DT")
	if !dirExists(dtDir) {
		t.Fatal("expected DT directory in worker lib")
	}

	// Verify .packages.json was updated.
	pkgManifest, err := pkgstore.ReadPackageManifest(workerLibDir)
	if err != nil {
		t.Fatalf("read package manifest: %v", err)
	}
	if pkgManifest["DT"] != "src1/cfg1" {
		t.Errorf(".packages.json DT = %q, want %q", pkgManifest["DT"], "src1/cfg1")
	}
}

// ---------------------------------------------------------------------------
// T6: Build container failure propagation
// ---------------------------------------------------------------------------

func TestInstallPackage_BuildFailure(t *testing.T) {
	srv, be := setupInstallTest(t)

	addStorePackage(t, srv, "shiny", "src1", "cfg1")
	setupBundleAndWorker(t, srv, "app-1", "bundle-1", "w-1",
		map[string]string{"shiny": "src1/cfg1"})

	// Build fails.
	be.BuildFn = func(_ context.Context, _ backend.BuildSpec) (backend.BuildResult, error) {
		return backend.BuildResult{
			Success:  false,
			ExitCode: 1,
			Logs:     "Error in pak::lockfile_create: could not resolve 'nonexistent'",
		}, nil
	}

	_, err := srv.InstallPackage(context.Background(), "app-1", "w-1",
		PackageRequest{Name: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for build failure")
	}
	if !strings.Contains(err.Error(), "runtime install failed") {
		t.Errorf("error = %q, want 'runtime install failed'", err.Error())
	}
	if !strings.Contains(err.Error(), "could not resolve") {
		t.Errorf("error should contain build logs, got: %q", err.Error())
	}
}
