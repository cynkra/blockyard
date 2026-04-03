//go:build docker_test

package preflight

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cynkra/blockyard/internal/backend/docker"
	"github.com/cynkra/blockyard/internal/config"
)

func testDockerClient(t *testing.T) *client.Client {
	t.Helper()
	cli, err := client.New(
		client.WithHost("unix:///var/run/docker.sock"),
	)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	if _, err := cli.Ping(context.Background(), client.PingOptions{}); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	return cli
}

func ensureAlpine(t *testing.T, ctx context.Context, cli *client.Client) {
	t.Helper()
	if err := ensureImage(ctx, cli, "alpine:latest"); err != nil {
		t.Fatalf("pull alpine: %v", err)
	}
}

// --- RunDockerChecks ---

func TestRunDockerChecks_AllPass(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	storePath := t.TempDir()
	ensureAlpine(t, ctx, cli)

	deps := DockerDeps{
		Client:    cli,
		MountCfg:  docker.MountConfig{Mode: docker.MountModeNative},
		Config:    &config.DockerConfig{Image: "alpine:latest"},
		StorePath: storePath,
	}

	report := RunDockerChecks(ctx, deps)
	if report == nil {
		t.Fatal("expected non-nil report")
	}
	for _, r := range report.Results {
		if r.Severity == SeverityError {
			t.Errorf("check %q failed: %s", r.Name, r.Message)
		}
	}
}

func TestRunDockerChecks_ImagePullFailure(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	deps := DockerDeps{
		Client:    cli,
		Config:    &config.DockerConfig{Image: "nonexistent-registry.invalid/no-such-image:never"},
		StorePath: t.TempDir(),
	}

	report := RunDockerChecks(ctx, deps)
	var foundImagePull bool
	for _, r := range report.Results {
		if r.Name == "image_pull" {
			foundImagePull = true
			if r.Severity != SeverityError {
				t.Errorf("image_pull severity = %v, want Error", r.Severity)
			}
		}
	}
	if !foundImagePull {
		t.Error("expected image_pull result in report")
	}
	// Should still run non-container checks (hardlink, metadata).
	var foundHardlink bool
	for _, r := range report.Results {
		if r.Name == "hardlink_cross_device" {
			foundHardlink = true
		}
	}
	if !foundHardlink {
		t.Error("expected hardlink check to still run after image pull failure")
	}
}

func TestRunDockerChecks_VolumeMode(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ensureAlpine(t, ctx, cli)

	deps := DockerDeps{
		Client:    cli,
		MountCfg:  docker.MountConfig{Mode: docker.MountModeVolume},
		Config:    &config.DockerConfig{Image: "alpine:latest"},
		StorePath: t.TempDir(),
	}

	report := RunDockerChecks(ctx, deps)
	// In volume mode, bind-mount checks (ro_bind, by_builder) are skipped.
	for _, r := range report.Results {
		if r.Name == "ro_bind_visibility" || r.Name == "by_builder_exec" {
			t.Errorf("check %q should not run in volume mode", r.Name)
		}
	}
}

// --- checkROBindVisibility ---

func TestCheckROBindVisibility(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	storePath := t.TempDir()
	ensureAlpine(t, ctx, cli)

	deps := DockerDeps{
		Client:    cli,
		MountCfg:  docker.MountConfig{Mode: docker.MountModeNative},
		Config:    &config.DockerConfig{Image: "alpine:latest"},
		StorePath: storePath,
	}

	res := checkROBindVisibility(ctx, deps)
	if res.Severity != SeverityOK {
		t.Errorf("severity = %v, want OK: %s", res.Severity, res.Message)
	}
}

// --- checkByBuilder ---

func TestCheckByBuilder(t *testing.T) {
	t.Run("skipped when binary empty", func(t *testing.T) {
		deps := DockerDeps{BuilderBin: ""}
		ctx := context.Background()
		res := checkByBuilder(ctx, deps)
		if res.Severity != SeverityOK {
			t.Errorf("severity = %v, want OK when BuilderBin is empty", res.Severity)
		}
	})

	t.Run("with valid script", func(t *testing.T) {
		cli := testDockerClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		ensureAlpine(t, ctx, cli)

		// Create a shell script that responds to --help.
		script := filepath.Join(t.TempDir(), "by-builder")
		os.WriteFile(script, []byte("#!/bin/sh\necho usage\n"), 0o755)

		deps := DockerDeps{
			Client:     cli,
			MountCfg:   docker.MountConfig{Mode: docker.MountModeNative},
			Config:     &config.DockerConfig{Image: "alpine:latest"},
			BuilderBin: script,
		}

		res := checkByBuilder(ctx, deps)
		if res.Severity != SeverityOK {
			t.Errorf("severity = %v, want OK: %s", res.Severity, res.Message)
		}
	})

	t.Run("with bad binary", func(t *testing.T) {
		cli := testDockerClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		ensureAlpine(t, ctx, cli)

		// Create a file that is not a valid executable.
		bad := filepath.Join(t.TempDir(), "by-builder")
		os.WriteFile(bad, []byte("not an executable"), 0o755)

		deps := DockerDeps{
			Client:     cli,
			MountCfg:   docker.MountConfig{Mode: docker.MountModeNative},
			Config:     &config.DockerConfig{Image: "alpine:latest"},
			BuilderBin: bad,
		}

		res := checkByBuilder(ctx, deps)
		if res.Severity != SeverityError {
			t.Errorf("severity = %v, want Error for bad binary: %s", res.Severity, res.Message)
		}
	})
}

// --- ensureImage ---

func TestEnsureImage_AlreadyPresent(t *testing.T) {
	cli := testDockerClient(t)
	ctx := context.Background()
	ensureAlpine(t, ctx, cli) // pre-pull
	// Second call should be a no-op (image inspect succeeds).
	if err := ensureImage(ctx, cli, "alpine:latest"); err != nil {
		t.Errorf("ensureImage for present image: %v", err)
	}
}

func TestEnsureImage_NonexistentImage(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := ensureImage(ctx, cli, "nonexistent-registry.invalid/no-such-image:never")
	if err == nil {
		t.Error("expected error for nonexistent image")
	}
}

// --- runEphemeralContainer ---

func TestRunEphemeralContainer(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ensureAlpine(t, ctx, cli)

	t.Run("echo", func(t *testing.T) {
		stdout, exitCode, err := runEphemeralContainer(ctx, cli,
			&container.Config{
				Image: "alpine:latest",
				Cmd:   []string{"echo", "hello-preflight"},
			},
			&container.HostConfig{},
			fmt.Sprintf("blockyard-test-eph-%d", time.Now().UnixNano()),
		)
		if err != nil {
			t.Fatalf("runEphemeralContainer: %v", err)
		}
		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0", exitCode)
		}
		if !strings.Contains(stdout, "hello-preflight") {
			t.Errorf("stdout = %q, want to contain 'hello-preflight'", stdout)
		}
	})

	t.Run("nonzero exit", func(t *testing.T) {
		_, exitCode, err := runEphemeralContainer(ctx, cli,
			&container.Config{
				Image: "alpine:latest",
				Cmd:   []string{"sh", "-c", "exit 42"},
			},
			&container.HostConfig{},
			fmt.Sprintf("blockyard-test-exit-%d", time.Now().UnixNano()),
		)
		if err != nil {
			t.Fatalf("runEphemeralContainer: %v", err)
		}
		if exitCode != 42 {
			t.Errorf("exit code = %d, want 42", exitCode)
		}
	})
}

// --- containerExec ---

func TestContainerExec(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ensureAlpine(t, ctx, cli)

	name := fmt.Sprintf("blockyard-test-exec-%d", time.Now().UnixNano())
	resp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: "alpine:latest",
			Cmd:   []string{"sleep", "30"},
		},
		Name: name,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer cli.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true}) //nolint:errcheck

	if _, err := cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}

	stdout, exitCode, err := containerExec(ctx, cli, resp.ID, []string{"echo", "exec-test"})
	if err != nil {
		t.Fatalf("containerExec: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout, "exec-test") {
		t.Errorf("stdout = %q, want to contain 'exec-test'", stdout)
	}
}

// --- containerLogs ---

func TestContainerLogs(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ensureAlpine(t, ctx, cli)

	name := fmt.Sprintf("blockyard-test-logs-%d", time.Now().UnixNano())
	resp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: "alpine:latest",
			Cmd:   []string{"echo", "logs-test-output"},
		},
		Name: name,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer cli.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true}) //nolint:errcheck

	if _, err := cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait for container to finish.
	waitResult := cli.ContainerWait(ctx, resp.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case <-waitResult.Result:
	case err := <-waitResult.Error:
		t.Fatalf("wait: %v", err)
	case <-ctx.Done():
		t.Fatal("timeout waiting for container")
	}

	logs, err := containerLogs(ctx, cli, resp.ID)
	if err != nil {
		t.Fatalf("containerLogs: %v", err)
	}
	if !strings.Contains(logs, "logs-test-output") {
		t.Errorf("logs = %q, want to contain 'logs-test-output'", logs)
	}
}

// --- checkHardLink (filesystem only, but gated here for coverage) ---

func TestCheckHardLink(t *testing.T) {
	t.Run("same filesystem passes", func(t *testing.T) {
		storePath := filepath.Join(t.TempDir(), ".pkg-store")
		os.MkdirAll(storePath, 0o755)
		res := checkHardLink(storePath)
		if res.Severity != SeverityOK {
			t.Errorf("severity = %v, want OK: %s", res.Severity, res.Message)
		}
	})
}
