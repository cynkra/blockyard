package bundle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cynkra/blockyard/internal/backend"
	mockmock "github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/pkgstore"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/telemetry"
)

// stageBuilderCache pre-populates a by-builder cache entry so that
// buildercache.EnsureCached returns immediately without attempting a
// compile-from-source fallback that would take seconds and require a
// Go toolchain on the worker.
func stageBuilderCache(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("by-builder-%s-linux-%s", version, runtime.GOARCH)
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/true\n"), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
}

// TestSpawnRestore_WithWaitGroup verifies that SpawnRestore calls
// WaitGroup.Add(1) before the goroutine and Done() on exit, so callers
// can synchronise server shutdown against in-flight restores.
func TestSpawnRestore_WithWaitGroup(t *testing.T) {
	params, tasks := setupRestoreTest(t, true)
	var wg sync.WaitGroup
	params.WG = &wg

	SpawnRestore(params)

	_, _, doneCh, ok := tasks.Subscribe("task-1")
	if !ok {
		t.Fatal("task not found")
	}
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SpawnRestore")
	}

	// After the task signals done, the WaitGroup's Done() must have fired.
	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("WaitGroup.Wait() did not return after task completion")
	}
}

// TestSpawnRestore_MetricsOnSuccess asserts that the success counter
// and build-duration histogram are touched on a successful restore.
func TestSpawnRestore_MetricsOnSuccess(t *testing.T) {
	params, tasks := setupRestoreTest(t, true)
	reg := prometheus.NewRegistry()
	params.Metrics = telemetry.NewMetrics(reg)

	SpawnRestore(params)

	_, _, doneCh, ok := tasks.Subscribe("task-1")
	if !ok {
		t.Fatal("task not found")
	}
	<-doneCh

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "blockyard_bundle_restores_succeeded_total" {
			for _, m := range mf.GetMetric() {
				if m.GetCounter().GetValue() == 1 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("succeeded counter was not incremented")
	}
}

// TestSpawnRestore_MetricsOnFailure asserts the failure counter is
// incremented when the build fails.
func TestSpawnRestore_MetricsOnFailure(t *testing.T) {
	params, tasks := setupRestoreTest(t, false)
	reg := prometheus.NewRegistry()
	params.Metrics = telemetry.NewMetrics(reg)

	SpawnRestore(params)

	_, _, doneCh, ok := tasks.Subscribe("task-1")
	if !ok {
		t.Fatal("task not found")
	}
	<-doneCh

	mfs, _ := reg.Gather()
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "blockyard_bundle_restores_failed_total" {
			for _, m := range mf.GetMetric() {
				if m.GetCounter().GetValue() == 1 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("failed counter was not incremented")
	}
}

// TestSpawnRestore_PanicRecovery verifies the deferred recover() path
// in SpawnRestore: a backend Build that panics must not crash the
// server; the task is marked Failed and the bundle row transitions to
// "failed".
func TestSpawnRestore_PanicRecovery(t *testing.T) {
	params, tasks := setupRestoreTest(t, true)
	be := params.Backend.(*mockmock.MockBackend)
	be.BuildFn = func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		panic("simulated backend crash")
	}

	SpawnRestore(params)

	_, _, doneCh, ok := tasks.Subscribe("task-1")
	if !ok {
		t.Fatal("task not found")
	}
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}

	status, _ := tasks.Status("task-1")
	if status != task.Failed {
		t.Errorf("status = %d, want Failed", status)
	}
	b, _ := params.DB.GetBundle("b-1")
	if b.Status != "failed" {
		t.Errorf("bundle status = %q, want failed", b.Status)
	}
}

// TestSpawnRestore_PanicRecovery_DBClosed exercises the error-logging
// branch in the panic-recovery defer when the failed-status update
// itself fails (DB connection gone).
func TestSpawnRestore_PanicRecovery_DBClosed(t *testing.T) {
	params, tasks := setupRestoreTest(t, true)
	database := params.DB
	be := params.Backend.(*mockmock.MockBackend)
	be.BuildFn = func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		_ = database.Close()
		panic("simulated backend crash")
	}

	SpawnRestore(params)

	_, _, doneCh, ok := tasks.Subscribe("task-1")
	if !ok {
		t.Fatal("task not found")
	}
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	if status, _ := tasks.Status("task-1"); status != task.Failed {
		t.Errorf("status = %d, want Failed", status)
	}
}

// TestSpawnRestore_BuildError_DBClosed exercises the error-logging
// branch in the non-panic error path when the failed-status update
// itself fails.
func TestSpawnRestore_BuildError_DBClosed(t *testing.T) {
	params, tasks := setupRestoreTest(t, true)
	database := params.DB
	be := params.Backend.(*mockmock.MockBackend)
	be.BuildFn = func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		_ = database.Close()
		return backend.BuildResult{}, fmt.Errorf("backend unreachable")
	}

	SpawnRestore(params)

	_, _, doneCh, ok := tasks.Subscribe("task-1")
	if !ok {
		t.Fatal("task not found")
	}
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	if status, _ := tasks.Status("task-1"); status != task.Failed {
		t.Errorf("status = %d, want Failed", status)
	}
}

// TestRunRestore_InvalidPakVersion covers the error branch when
// pakcache.EnsureInstalled rejects the version string.
func TestRunRestore_InvalidPakVersion(t *testing.T) {
	params, _ := setupRestoreTest(t, true)
	params.PakVersion = "not-a-channel"

	err := runRestore(params)
	if err == nil {
		t.Fatal("expected error for invalid pak_version")
	}
}

// TestRunRestore_BareScriptsFailure removes the manifest so that
// runRestore falls through to preProcess, and makes the Build call fail
// during preprocess — covering the preprocess error path.
func TestRunRestore_BareScriptsFailure(t *testing.T) {
	params, _ := setupRestoreTest(t, true)
	// Remove manifest so resolveManifest returns nil.
	os.Remove(filepath.Join(params.Paths.Unpacked, "manifest.json"))

	be := params.Backend.(*mockmock.MockBackend)
	calls := 0
	be.BuildFn = func(_ context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
		calls++
		// First call = pak install; succeed. Second call = preprocess; fail.
		if calls == 1 {
			return backend.BuildResult{Success: true}, nil
		}
		return backend.BuildResult{Success: false, ExitCode: 1, Logs: "boom"}, nil
	}

	err := runRestore(params)
	if err == nil {
		t.Fatal("expected error from failed preprocess")
	}
}

// TestRunRestore_BuildBackendError exercises the path where
// Backend.Build returns a non-nil error (distinct from a non-success
// result).
func TestRunRestore_BuildBackendError(t *testing.T) {
	params, _ := setupRestoreTest(t, true)
	be := params.Backend.(*mockmock.MockBackend)
	be.BuildFn = func(_ context.Context, _ backend.BuildSpec) (backend.BuildResult, error) {
		return backend.BuildResult{}, fmt.Errorf("backend unreachable")
	}

	err := runRestore(params)
	if err == nil || err.Error() == "" {
		t.Fatal("expected build error")
	}
}

// TestRunRestore_BuildTimeout covers the branch that distinguishes a
// context-deadline timeout from a plain build failure.
func TestRunRestore_BuildTimeout(t *testing.T) {
	params, _ := setupRestoreTest(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancel so ctx.Err() is non-nil by the time Build returns
	params.Ctx = ctx

	be := params.Backend.(*mockmock.MockBackend)
	be.BuildFn = func(_ context.Context, _ backend.BuildSpec) (backend.BuildResult, error) {
		// Simulate "container killed" from a cancelled context.
		return backend.BuildResult{Success: false, ExitCode: -1}, nil
	}

	err := runRestore(params)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// TestRunRestore_StoreAwareMissingArtifacts exercises the
// store-aware branch with a successful build but no store-manifest.json
// written — the copyFile step must surface an error.
func TestRunRestore_StoreAwareMissingArtifacts(t *testing.T) {
	params, _ := setupRestoreTest(t, true)

	storeRoot := filepath.Join(t.TempDir(), "store")
	params.Store = pkgstore.NewStore(storeRoot)
	builderCache := filepath.Join(t.TempDir(), "builder-cache")
	stageBuilderCache(t, builderCache, "")
	params.BuilderCachePath = builderCache

	// Build reports success but does NOT populate the build dir with
	// store-manifest.json — the copyFile call should fail.
	be := params.Backend.(*mockmock.MockBackend)
	be.BuildFn = func(_ context.Context, _ backend.BuildSpec) (backend.BuildResult, error) {
		return backend.BuildResult{Success: true}, nil
	}

	err := runRestore(params)
	if err == nil {
		t.Fatal("expected error when store-manifest.json is missing after build")
	}
}

// TestRunRestore_StoreAwareSuccess covers the happy path when Store is
// wired, including lockfile read and manifest persistence to the base
// dir. Requires staging both the builder cache and the build dir with
// the expected artifacts.
func TestRunRestore_StoreAwareSuccess(t *testing.T) {
	params, _ := setupRestoreTest(t, true)

	storeRoot := filepath.Join(t.TempDir(), "store")
	params.Store = pkgstore.NewStore(storeRoot)
	builderCache := filepath.Join(t.TempDir(), "builder-cache")
	stageBuilderCache(t, builderCache, "")
	params.BuilderCachePath = builderCache

	// The BuildFn is invoked twice: once for pak install (ignore), once
	// for the actual build. For the second, write a store-manifest.json
	// into the build dir so copyFile succeeds.
	be := params.Backend.(*mockmock.MockBackend)
	be.BuildFn = func(_ context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
		// Identify the app build (not pak install) by AppID.
		if spec.AppID == params.AppID {
			var buildUUID string
			for _, env := range spec.Env {
				if len(env) > len("BUILD_UUID=") && env[:len("BUILD_UUID=")] == "BUILD_UUID=" {
					buildUUID = env[len("BUILD_UUID="):]
				}
			}
			if buildUUID != "" {
				buildDir := filepath.Join(storeRoot, ".builds", buildUUID)
				os.MkdirAll(buildDir, 0o755)
				os.WriteFile(filepath.Join(buildDir, "store-manifest.json"),
					[]byte(`{"packages":[]}`), 0o644)
				os.WriteFile(filepath.Join(buildDir, "pak.lock"),
					[]byte(`{"packages":[]}`), 0o644)
			}
		}
		return backend.BuildResult{Success: true}, nil
	}

	if err := runRestore(params); err != nil {
		t.Fatalf("runRestore: %v", err)
	}

	// store-manifest.json should now live in the bundle base dir.
	if _, err := os.Stat(filepath.Join(params.Paths.Base, "store-manifest.json")); err != nil {
		t.Errorf("store-manifest.json not copied to base: %v", err)
	}
}

// TestRunRestore_ActivateBundleFailure makes the bundle not exist in the
// DB before runRestore so ActivateBundle's SQL update fails.
func TestRunRestore_ActivateBundleFailure(t *testing.T) {
	params, _ := setupRestoreTest(t, true)
	// Force UpdateBundleStatus to succeed for "building" then have the
	// bundle row disappear before ActivateBundle. Simulate by deleting
	// the app row after setup — ActivateBundle checks for the
	// app/bundle pair and returns ErrNoRows.
	if _, err := params.DB.DeleteBundle("b-1"); err != nil {
		t.Fatal(err)
	}

	err := runRestore(params)
	if err == nil {
		t.Fatal("expected ActivateBundle to fail for missing bundle")
	}
}

// TestSpawnRestore_FailsIfAllCompleteFlowUsedNilMetrics verifies that
// not setting Metrics still works — the nil-guard branches.
func TestSpawnRestore_NilMetricsStillSucceeds(t *testing.T) {
	params, tasks := setupRestoreTest(t, true)
	params.Metrics = nil

	SpawnRestore(params)
	_, _, doneCh, _ := tasks.Subscribe("task-1")
	<-doneCh
	if s, _ := tasks.Status("task-1"); s != task.Completed {
		t.Errorf("task status = %d, want Completed", s)
	}
}

// TestRunRestore_BadManifest covers the path where manifest.json exists
// but is malformed — resolveManifest returns an error that must surface
// from runRestore with the "resolve manifest" prefix.
func TestRunRestore_BadManifest(t *testing.T) {
	params, _ := setupRestoreTest(t, true)
	// Overwrite the manifest with invalid JSON.
	path := filepath.Join(params.Paths.Unpacked, "manifest.json")
	if err := os.WriteFile(path, []byte("{not valid"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runRestore(params)
	if err == nil {
		t.Fatal("expected resolveManifest error for malformed manifest.json")
	}
}

// TestRunRestore_DefaultPakCachePath covers the branch where
// RestoreParams.PakCachePath is empty and the default
// {BasePath}/.pak-cache is computed.
func TestRunRestore_DefaultPakCachePath(t *testing.T) {
	params, _ := setupRestoreTest(t, true)
	// Pre-populate the default location so pakcache.EnsureInstalled short-circuits.
	defaultCache := filepath.Join(params.BasePath, ".pak-cache")
	if err := os.MkdirAll(filepath.Join(defaultCache, "pak-stable"), 0o755); err != nil {
		t.Fatal(err)
	}
	params.PakCachePath = ""

	if err := runRestore(params); err != nil {
		t.Fatalf("runRestore with default pak cache: %v", err)
	}
}

// TestRunRestore_DefaultBuilderCachePath covers the store-aware branch
// where BuilderCachePath is empty and the default path is computed
// from BasePath.
func TestRunRestore_DefaultBuilderCachePath(t *testing.T) {
	params, _ := setupRestoreTest(t, true)
	storeRoot := filepath.Join(t.TempDir(), "store")
	params.Store = pkgstore.NewStore(storeRoot)

	// Stage builder cache at the default location.
	defaultBuilderDir := filepath.Join(params.BasePath, ".by-builder-cache")
	stageBuilderCache(t, defaultBuilderDir, "")
	params.BuilderCachePath = ""

	be := params.Backend.(*mockmock.MockBackend)
	be.BuildFn = func(_ context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
		if spec.AppID == params.AppID {
			for _, env := range spec.Env {
				if len(env) > len("BUILD_UUID=") && env[:len("BUILD_UUID=")] == "BUILD_UUID=" {
					uuidVal := env[len("BUILD_UUID="):]
					buildDir := filepath.Join(storeRoot, ".builds", uuidVal)
					os.MkdirAll(buildDir, 0o755)
					os.WriteFile(filepath.Join(buildDir, "store-manifest.json"),
						[]byte(`{"packages":[]}`), 0o644)
					os.WriteFile(filepath.Join(buildDir, "pak.lock"),
						[]byte(`{"packages":[]}`), 0o644)
				}
			}
		}
		return backend.BuildResult{Success: true}, nil
	}

	if err := runRestore(params); err != nil {
		t.Fatalf("runRestore with default builder cache: %v", err)
	}
}

// TestRunRestore_LegacyManifestFromRenvLock exercises the legacy branch
// where resolveManifest parses renv.lock (no manifest.json), writes
// manifest.json, .pak-refs and .pak-repos. This covers the write paths
// at lines 224-240.
func TestRunRestore_LegacyManifestFromRenvLock(t *testing.T) {
	params, _ := setupRestoreTest(t, true)
	// Drop the manifest so resolveManifest falls through to renv.lock.
	os.Remove(filepath.Join(params.Paths.Unpacked, "manifest.json"))
	// Write a minimal renv.lock that manifest.FromRenvLock accepts.
	renvLock := `{
	"R": {"Version": "4.4.2", "Repositories": [{"Name": "CRAN", "URL": "https://cran.r-project.org"}]},
	"Packages": {
		"shiny": {"Package": "shiny", "Version": "1.8.0", "Source": "Repository", "Repository": "CRAN"}
	}
}`
	if err := os.WriteFile(filepath.Join(params.Paths.Unpacked, "renv.lock"),
		[]byte(renvLock), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runRestore(params); err != nil {
		t.Fatalf("runRestore: %v", err)
	}
	// Both metadata files should now be on disk.
	for _, name := range []string{"manifest.json", ".pak-refs", ".pak-repos"} {
		if _, err := os.Stat(filepath.Join(params.Paths.Unpacked, name)); err != nil {
			t.Errorf("%s not written: %v", name, err)
		}
	}
}

// TestRunRestore_LegacyBareScriptsSucceeds exercises the post-preprocess
// resolveManifest+Write path in the legacy branch. preProcess writes a
// DESCRIPTION into /output via the mock backend; the second
// resolveManifest call picks it up.
func TestRunRestore_LegacyBareScriptsSucceeds(t *testing.T) {
	params, _ := setupRestoreTest(t, true)
	// Bare scripts: no manifest.json and no renv.lock / DESCRIPTION.
	os.Remove(filepath.Join(params.Paths.Unpacked, "manifest.json"))

	// setupRestoreTest pre-populates the pak cache, so Build is only called
	// for preprocess and the real build. Detect the preprocess call by its
	// distinctive /output mount and write the synthetic DESCRIPTION there.
	be := params.Backend.(*mockmock.MockBackend)
	be.BuildFn = func(_ context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
		for _, m := range spec.Mounts {
			if m.Target == "/output" {
				_ = os.WriteFile(filepath.Join(m.Source, "DESCRIPTION"),
					[]byte("Package: app\nVersion: 0.0.1\nImports: shiny\n"), 0o644)
			}
		}
		return backend.BuildResult{Success: true}, nil
	}

	if err := runRestore(params); err != nil {
		t.Fatalf("runRestore bare-scripts: %v", err)
	}
}

// TestRunRestore_StoreAwareWithExistingManifest covers the
// `!fileExists(manifestPath)` branch in the store-aware path being
// false (manifest.json is already on disk, so Write is skipped).
func TestRunRestore_StoreAwareWithExistingManifest(t *testing.T) {
	params, _ := setupRestoreTest(t, true)
	storeRoot := filepath.Join(t.TempDir(), "store")
	params.Store = pkgstore.NewStore(storeRoot)
	builderCache := filepath.Join(t.TempDir(), "builder-cache")
	stageBuilderCache(t, builderCache, "")
	params.BuilderCachePath = builderCache

	be := params.Backend.(*mockmock.MockBackend)
	be.BuildFn = func(_ context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
		if spec.AppID == params.AppID {
			for _, env := range spec.Env {
				if len(env) > len("BUILD_UUID=") && env[:len("BUILD_UUID=")] == "BUILD_UUID=" {
					uuidVal := env[len("BUILD_UUID="):]
					buildDir := filepath.Join(storeRoot, ".builds", uuidVal)
					os.MkdirAll(buildDir, 0o755)
					os.WriteFile(filepath.Join(buildDir, "store-manifest.json"),
						[]byte(`{"packages":[]}`), 0o644)
				}
			}
		}
		return backend.BuildResult{Success: true}, nil
	}

	if err := runRestore(params); err != nil {
		t.Fatalf("runRestore: %v", err)
	}
}
