package preflight

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/cynkra/blockyard/internal/backend/docker"
	"github.com/cynkra/blockyard/internal/config"
)

// DockerDeps holds dependencies for Docker-dependent preflight checks.
type DockerDeps struct {
	Client     *client.Client
	ServerID   string // container ID of server; empty = native mode
	MountCfg   docker.MountConfig
	Config     *config.DockerConfig
	StorePath  string // server-side .pkg-store root
	BuilderBin string // path to cached by-builder binary (empty = skip check)
}

// RunDockerChecks evaluates checks that require Docker or filesystem
// operations. The context should carry a timeout (recommended: 2min).
func RunDockerChecks(ctx context.Context, deps DockerDeps) *Report {
	r := &Report{}

	// Ensure the configured image is available locally before running
	// container-based checks.
	if err := ensureImage(ctx, deps.Client, deps.Config.Image); err != nil {
		r.add(&Result{
			Name:     "image_pull",
			Severity: SeverityError,
			Message:  fmt.Sprintf("failed to pull worker image %q: %v", deps.Config.Image, err),
		})
		// Container-based checks cannot proceed without the image;
		// still run non-container checks.
		r.add(checkHardLink(deps.StorePath))
		r.add(checkMetadataBlocking(deps.ServerID))
		return r
	}

	// Container-based checks that use bind mounts are not applicable in
	// volume mode — volumes use subpath mounts instead.
	if deps.MountCfg.Mode != docker.MountModeVolume {
		r.add(checkROBindVisibility(ctx, deps))
		r.add(checkByBuilder(ctx, deps))
	}
	r.add(checkHardLink(deps.StorePath))
	r.add(checkMetadataBlocking(deps.ServerID))

	return r
}

// checkROBindVisibility verifies that host-side writes to a read-only
// bind mount are visible inside a running container. This relies on
// standard Linux VFS behavior that can break with certain Docker
// storage drivers or rootless configurations.
func checkROBindVisibility(ctx context.Context, deps DockerDeps) *Result {
	// Create temp dir under the store path (which is on a known-good
	// mount), not /tmp — in container mode, /tmp is container-local
	// and cannot be bind-mounted into sibling containers.
	tmpDir, err := os.MkdirTemp(deps.StorePath, ".preflight-ro-*")
	if err != nil {
		return &Result{
			Name:     "ro_bind_visibility",
			Severity: SeverityError,
			Message:  fmt.Sprintf("failed to create temp dir: %v", err),
		}
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck // best-effort cleanup

	// Build the mount for the temp directory using the same mount
	// translation the backend uses for worker containers.
	containerName := "blockyard-preflight-ro-" + fmt.Sprintf("%d", time.Now().UnixNano())

	var hostCfg *container.HostConfig
	switch deps.MountCfg.Mode {
	case docker.MountModeBind:
		hostCfg = &container.HostConfig{
			Binds: []string{deps.MountCfg.ToHostPath(tmpDir) + ":/preflight-test:ro"},
		}
	default: // Native
		hostCfg = &container.HostConfig{
			Binds: []string{tmpDir + ":/preflight-test:ro"},
		}
	}

	// Start a container that sleeps, bind-mounting the empty dir read-only.
	resp, err := deps.Client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: deps.Config.Image,
			Cmd:   []string{"sleep", "30"},
		},
		HostConfig: hostCfg,
		Name:       containerName,
	})
	if err != nil {
		return &Result{
			Name:     "ro_bind_visibility",
			Severity: SeverityError,
			Message:  fmt.Sprintf("failed to create test container: %v", err),
		}
	}
	defer deps.Client.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true}) //nolint:errcheck

	if _, err := deps.Client.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		return &Result{
			Name:     "ro_bind_visibility",
			Severity: SeverityError,
			Message:  fmt.Sprintf("failed to start test container: %v", err),
		}
	}

	// Write a file on the host side while the container is running.
	sentinel := filepath.Join(tmpDir, "preflight-sentinel")
	if err := os.WriteFile(sentinel, []byte("ok"), 0o644); err != nil { //nolint:gosec // G306: preflight sentinel file, not secrets
		return &Result{
			Name:     "ro_bind_visibility",
			Severity: SeverityError,
			Message:  fmt.Sprintf("failed to write sentinel file: %v", err),
		}
	}

	// Exec into the container to check visibility.
	stdout, exitCode, err := containerExec(ctx, deps.Client, resp.ID,
		[]string{"cat", "/preflight-test/preflight-sentinel"})
	if err != nil || exitCode != 0 || stdout != "ok" {
		return &Result{
			Name:     "ro_bind_visibility",
			Severity: SeverityError,
			Message: "host-side writes to a read-only bind mount are not visible inside containers; " +
				"this breaks runtime package installation (check Docker storage driver and rootless config)",
		}
	}

	return nil
}

// checkHardLink verifies that hard links work between the package store
// root and the per-worker library directory. Both must reside on the
// same filesystem.
func checkHardLink(storePath string) *Result {
	workersDir := filepath.Join(storePath, ".workers")
	if err := os.MkdirAll(workersDir, 0o755); err != nil { //nolint:gosec // G301: workers dir, not secrets
		return &Result{
			Name:     "hardlink_cross_device",
			Severity: SeverityError,
			Message:  fmt.Sprintf("failed to create workers dir: %v", err),
		}
	}

	// Create a temp file in the store root.
	src, err := os.CreateTemp(storePath, "preflight-link-*")
	if err != nil {
		return &Result{
			Name:     "hardlink_cross_device",
			Severity: SeverityError,
			Message:  fmt.Sprintf("failed to create test file in store: %v", err),
		}
	}
	src.Close()
	defer os.Remove(src.Name())

	// Try to hard-link it into the workers directory.
	dst := filepath.Join(workersDir, filepath.Base(src.Name()))
	err = os.Link(src.Name(), dst)
	if err != nil {
		os.Remove(dst)
		return &Result{
			Name:     "hardlink_cross_device",
			Severity: SeverityError,
			Message: "hard links between .pkg-store and .pkg-store/.workers fail " +
				"(cross-device link); both must be on the same filesystem",
		}
	}
	os.Remove(dst)

	return nil
}

// checkByBuilder verifies that the by-builder binary is executable and
// the correct architecture by running it in a short-lived container.
func checkByBuilder(ctx context.Context, deps DockerDeps) *Result {
	if deps.BuilderBin == "" {
		return nil // skip when binary is not available
	}

	// Resolve host-side path for bind mount.
	hostBin := deps.BuilderBin
	if deps.MountCfg.Mode == docker.MountModeBind {
		hostBin = deps.MountCfg.ToHostPath(deps.BuilderBin)
	}

	containerName := "blockyard-preflight-builder-" + fmt.Sprintf("%d", time.Now().UnixNano())
	stdout, exitCode, err := runEphemeralContainer(ctx, deps.Client,
		&container.Config{
			Image: deps.Config.Image,
			Cmd:   []string{"/tools/by-builder", "--help"},
		},
		&container.HostConfig{
			Binds: []string{hostBin + ":/tools/by-builder:ro"},
		},
		containerName,
	)

	if err != nil {
		return &Result{
			Name:     "by_builder_exec",
			Severity: SeverityError,
			Message:  fmt.Sprintf("failed to run by-builder check: %v", err),
		}
	}
	if exitCode != 0 {
		detail := stdout
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return &Result{
			Name:     "by_builder_exec",
			Severity: SeverityError,
			Message: fmt.Sprintf("by-builder binary failed (exit %d); "+
				"check architecture matches the container image: %s", exitCode, detail),
		}
	}

	return nil
}

// checkMetadataBlocking probes whether the server can block container
// access to the cloud metadata endpoint (169.254.169.254). This
// requires either CAP_NET_ADMIN or an existing host-level iptables
// rule.
func checkMetadataBlocking(serverID string) *Result {
	// Check if a blanket rule already exists.
	if docker.DockerUserBlocksMetadata() {
		return nil
	}

	// In container mode, also try a TCP connect test: if the metadata
	// endpoint is unreachable, it is already blocked.
	if serverID != "" {
		conn, err := net.DialTimeout("tcp", "169.254.169.254:80", 2*time.Second)
		if err != nil {
			return nil // unreachable = blocked
		}
		conn.Close()
	}

	// Probe whether we can manipulate iptables at all.
	if err := exec.Command("iptables", "-S", "DOCKER-USER").Run(); err != nil {
		return &Result{
			Name:     "metadata_endpoint",
			Severity: SeverityWarning,
			Message: "cannot block cloud metadata endpoint (169.254.169.254); " +
				"grant CAP_NET_ADMIN to the server container, or add a host-level rule: " +
				"iptables -I DOCKER-USER -d 169.254.169.254/32 -j DROP",
		}
	}

	return nil
}

// --- helpers ---

// ensureImage pulls a Docker image if it is not already present locally.
func ensureImage(ctx context.Context, cli *client.Client, img string) error {
	_, err := cli.ImageInspect(ctx, img)
	if err == nil {
		return nil
	}

	slog.Info("preflight: pulling image", "image", img)
	pullResp, err := cli.ImagePull(ctx, img, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", img, err)
	}
	defer pullResp.Close()
	if _, err := io.Copy(io.Discard, pullResp); err != nil {
		return fmt.Errorf("pull %s: %w", img, err)
	}
	return nil
}

// runEphemeralContainer creates, starts, waits for, and removes a
// container. Returns combined stdout/stderr and exit code.
func runEphemeralContainer(
	ctx context.Context,
	cli *client.Client,
	cfg *container.Config,
	hostCfg *container.HostConfig,
	name string,
) (string, int, error) {
	resp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     cfg,
		HostConfig: hostCfg,
		Name:       name,
	})
	if err != nil {
		return "", -1, fmt.Errorf("create container: %w", err)
	}
	defer cli.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true}) //nolint:errcheck

	if _, err := cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		return "", -1, fmt.Errorf("start container: %w", err)
	}

	waitResult := cli.ContainerWait(ctx, resp.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case result := <-waitResult.Result:
		output, _ := containerLogs(ctx, cli, resp.ID)
		return output, int(result.StatusCode), nil
	case err := <-waitResult.Error:
		return "", -1, fmt.Errorf("wait: %w", err)
	case <-ctx.Done():
		return "", -1, ctx.Err()
	}
}

// containerExec runs a command inside a running container and returns
// stdout, exit code, and any error.
func containerExec(ctx context.Context, cli *client.Client, containerID string, cmd []string) (string, int, error) {
	execResult, err := cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", -1, err
	}

	attach, err := cli.ExecAttach(ctx, execResult.ID, client.ExecAttachOptions{})
	if err != nil {
		return "", -1, err
	}
	defer attach.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attach.Reader); err != nil {
		// Ignore errors from copy — we may still have partial output.
		_ = err
	}

	inspect, err := cli.ExecInspect(ctx, execResult.ID, client.ExecInspectOptions{})
	if err != nil {
		return stdout.String(), -1, err
	}

	return stdout.String(), inspect.ExitCode, nil
}

// containerLogs fetches the combined stdout/stderr of a stopped container.
func containerLogs(ctx context.Context, cli *client.Client, containerID string) (string, error) {
	reader, err := cli.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		return "", err
	}
	defer reader.Close()

	var stdout, stderr bytes.Buffer
	_, _ = stdcopy.StdCopy(&stdout, &stderr, reader)

	var combined bytes.Buffer
	combined.Write(stdout.Bytes())
	combined.Write(stderr.Bytes())
	return combined.String(), nil
}

// compile-time check that MountConfig has the methods we need.
var _ interface {
	ToHostPath(string) string
} = docker.MountConfig{}
