//go:build docker_test

package docker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/preflight"
)

// TestCheckROBindVisibility_BindMode hits the MountModeBind switch
// branch inside checkROBindVisibility that translates the host-side
// temp directory via MountConfig.ToHostPath.
//
// This complements the existing Native-mode test at
// preflight_test.go:TestCheckROBindVisibility.
func TestCheckROBindVisibility_BindMode(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	storePath := t.TempDir()
	ensureAlpine(t, ctx, cli)

	d := newPreflightTestBackend(t, cli, "alpine:latest", MountModeBind)
	// Configure the mount translation so HostPath(serverPath) passes
	// unchanged — storePath is already the host path in this test
	// environment, and ToHostPath(MountDest + subpath) = HostSource +
	// subpath. With both set equal to storePath, the translation is a
	// no-op and the check exercises the bind-mount code path without
	// relying on a real mount-point discovery.
	d.mountCfg.HostSource = storePath
	d.mountCfg.MountDest = storePath
	deps := PreflightDeps{StorePath: storePath}

	res := checkROBindVisibility(ctx, d, deps)
	if res.Severity != preflight.SeverityOK {
		t.Errorf("severity = %v, want OK: %s", res.Severity, res.Message)
	}
}

// TestCheckByBuilder_BindMode exercises the MountModeBind branch in
// checkByBuilder where the builder-binary path is translated through
// MountConfig.ToHostPath before the container bind mount is created.
func TestCheckByBuilder_BindMode(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ensureAlpine(t, ctx, cli)

	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "by-builder")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho usage\n"), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}

	d := newPreflightTestBackend(t, cli, "alpine:latest", MountModeBind)
	d.mountCfg.HostSource = scriptDir
	d.mountCfg.MountDest = scriptDir
	deps := PreflightDeps{BuilderBin: script}

	res := checkByBuilder(ctx, d, deps)
	if res.Severity != preflight.SeverityOK {
		t.Errorf("severity = %v, want OK: %s", res.Severity, res.Message)
	}
}

// TestRunEphemeralContainer_CreateFailure covers the error-return path
// in runEphemeralContainer when ContainerCreate rejects the config —
// here we use an invalid image reference so Docker returns an error
// before the container is started.
func TestRunEphemeralContainer_CreateFailure(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// An empty Image string is not a valid reference — ContainerCreate
	// errors immediately.
	_, exit, err := runEphemeralContainer(ctx, cli,
		nil, nil,
		"blockyard-test-invalid",
	)
	if err == nil {
		t.Fatal("expected error for nil container Config")
	}
	if exit != -1 {
		t.Errorf("exitCode = %d, want -1 on create failure", exit)
	}
}
