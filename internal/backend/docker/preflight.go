package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cynkra/blockyard/internal/buildercache"
	"github.com/cynkra/blockyard/internal/dockerauth"
	"github.com/cynkra/blockyard/internal/preflight"
)

// PreflightDeps holds the runtime values that the docker-backend
// preflight checks need but cannot derive from the backend itself
// (paths, builder cache location, optional Redis URL).
type PreflightDeps struct {
	StorePath  string
	BuilderBin string
	RedisURL   string
	Version    string
}

// CheckRVersion returns nil — the Docker backend selects R via image tag.
func (d *DockerBackend) CheckRVersion(_ string) error { return nil }

// Preflight implements backend.Backend. It runs all docker-specific
// preflight checks and returns a populated *preflight.Report. The full
// list of checks lives in this file as runDockerChecks; this method is
// the entry point that main.go calls through the Backend interface.
func (d *DockerBackend) Preflight(ctx context.Context) (*preflight.Report, error) {
	storePath := filepath.Join(d.bundleServerPath, ".pkg-store")
	builderBin, builderErr := buildercache.EnsureCached(
		filepath.Join(d.bundleServerPath, ".by-builder-cache"), d.serverVersion)
	if builderErr != nil {
		slog.Warn("preflight: could not cache by-builder, skipping builder check",
			"error", builderErr)
		builderBin = ""
	}
	deps := PreflightDeps{
		StorePath:  storePath,
		BuilderBin: builderBin,
		RedisURL:   d.redisURL,
		Version:    d.serverVersion,
	}
	return runDockerChecks(ctx, d, deps), nil
}

// CleanupOrphanResources implements backend.Backend. Removes blockyard
// iptables rules left over from previous runs.
func (d *DockerBackend) CleanupOrphanResources(_ context.Context) error {
	cleanupOrphanMetadataRulesWithRunner(context.Background(), d.runCmd)
	return nil
}

// runDockerChecks evaluates checks that require Docker or filesystem
// operations. The context should carry a timeout (recommended: 2min).
func runDockerChecks(ctx context.Context, d *DockerBackend, deps PreflightDeps) *preflight.Report {
	r := &preflight.Report{RanAt: time.Now().UTC()}

	// Ensure the configured image is available locally before running
	// container-based checks.
	if err := ensurePreflightImage(ctx, d.Client(), d.config.Image); err != nil {
		r.Add(preflight.Result{
			Name:     "image_pull",
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("failed to pull worker image %q: %v", d.config.Image, err),
			Category: "docker",
		})
		// Container-based checks cannot proceed without the image;
		// still run non-container checks.
		r.Add(checkHardLink(deps.StorePath))
		r.Add(checkMetadataBlocking(d.serverID))
		r.Add(preflight.CheckRedisAuth(d.fullCfg.Redis))
		return r
	}

	// Container-based checks that use bind mounts are not applicable in
	// volume mode — volumes use subpath mounts instead.
	if d.mountCfg.Mode != MountModeVolume {
		r.Add(checkROBindVisibility(ctx, d, deps))
		r.Add(checkByBuilder(ctx, d, deps))
	}
	r.Add(checkHardLink(deps.StorePath))
	r.Add(checkMetadataBlocking(d.serverID))
	r.Add(checkRedisOnServiceNetwork(ctx, d, deps))
	r.Add(preflight.CheckRedisAuth(d.fullCfg.Redis))

	return r
}

// checkROBindVisibility verifies that host-side writes to a read-only
// bind mount are visible inside a running container. This relies on
// standard Linux VFS behavior that can break with certain Docker
// storage drivers or rootless configurations.
func checkROBindVisibility(ctx context.Context, d *DockerBackend, deps PreflightDeps) preflight.Result {
	const name = "ro_bind_visibility"
	const category = "docker"

	// Create temp dir under the store path (which is on a known-good
	// mount), not /tmp — in container mode, /tmp is container-local
	// and cannot be bind-mounted into sibling containers.
	tmpDir, err := os.MkdirTemp(deps.StorePath, ".preflight-ro-*")
	if err != nil {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("failed to create temp dir: %v", err),
			Category: category,
		}
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck // best-effort cleanup

	// Build the mount for the temp directory using the same mount
	// translation the backend uses for worker containers.
	containerName := "blockyard-preflight-ro-" + fmt.Sprintf("%d", time.Now().UnixNano())

	var hostCfg *container.HostConfig
	switch d.mountCfg.Mode {
	case MountModeBind:
		hostCfg = &container.HostConfig{
			Binds: []string{d.mountCfg.ToHostPath(tmpDir) + ":/preflight-test:ro"},
		}
	default: // Native
		hostCfg = &container.HostConfig{
			Binds: []string{tmpDir + ":/preflight-test:ro"},
		}
	}

	// Start a container that sleeps, bind-mounting the empty dir read-only.
	resp, err := d.Client().ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: d.config.Image,
			Cmd:   []string{"sleep", "30"},
		},
		HostConfig: hostCfg,
		Name:       containerName,
	})
	if err != nil {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("failed to create test container: %v", err),
			Category: category,
		}
	}
	defer d.Client().ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true}) //nolint:errcheck

	if _, err := d.Client().ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("failed to start test container: %v", err),
			Category: category,
		}
	}

	// Write a file on the host side while the container is running.
	sentinel := filepath.Join(tmpDir, "preflight-sentinel")
	if err := os.WriteFile(sentinel, []byte("ok"), 0o644); err != nil { //nolint:gosec // G306: preflight sentinel file, not secrets
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("failed to write sentinel file: %v", err),
			Category: category,
		}
	}

	// Exec into the container to check visibility.
	stdout, exitCode, err := containerExec(ctx, d.Client(), resp.ID,
		[]string{"cat", "/preflight-test/preflight-sentinel"})
	if err != nil || exitCode != 0 || stdout != "ok" {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message: "host-side writes to a read-only bind mount are not visible inside containers; " +
				"this breaks runtime package installation (check Docker storage driver and rootless config)",
			Category: category,
		}
	}

	return preflight.Result{
		Name:     name,
		Severity: preflight.SeverityOK,
		Message:  "read-only bind mount visibility is working",
		Category: category,
	}
}

// checkHardLink verifies that hard links work between the package store
// root and the per-worker library directory. Both must reside on the
// same filesystem.
func checkHardLink(storePath string) preflight.Result {
	const name = "hardlink_cross_device"
	const category = "docker"

	workersDir := filepath.Join(storePath, ".workers")
	if err := os.MkdirAll(workersDir, 0o755); err != nil { //nolint:gosec // G301: workers dir, not secrets
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("failed to create workers dir: %v", err),
			Category: category,
		}
	}

	// Create a temp file in the store root.
	src, err := os.CreateTemp(storePath, "preflight-link-*")
	if err != nil {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("failed to create test file in store: %v", err),
			Category: category,
		}
	}
	src.Close()
	defer os.Remove(src.Name())

	// Try to hard-link it into the workers directory.
	dst := filepath.Join(workersDir, filepath.Base(src.Name()))
	err = os.Link(src.Name(), dst)
	if err != nil {
		os.Remove(dst)
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message: "hard links between .pkg-store and .pkg-store/.workers fail " +
				"(cross-device link); both must be on the same filesystem",
			Category: category,
		}
	}
	os.Remove(dst)

	return preflight.Result{
		Name:     name,
		Severity: preflight.SeverityOK,
		Message:  "hard links between store and workers directory are working",
		Category: category,
	}
}

// checkByBuilder verifies that the by-builder binary is executable and
// the correct architecture by running it in a short-lived container.
func checkByBuilder(ctx context.Context, d *DockerBackend, deps PreflightDeps) preflight.Result {
	const name = "by_builder_exec"
	const category = "docker"

	if deps.BuilderBin == "" {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityOK,
			Message:  "by-builder binary not available; check skipped",
			Category: category,
		}
	}

	// Resolve host-side path for bind mount.
	hostBin := deps.BuilderBin
	if d.mountCfg.Mode == MountModeBind {
		hostBin = d.mountCfg.ToHostPath(deps.BuilderBin)
	}

	containerName := "blockyard-preflight-builder-" + fmt.Sprintf("%d", time.Now().UnixNano())
	stdout, exitCode, err := runEphemeralContainer(ctx, d.Client(),
		&container.Config{
			Image: d.config.Image,
			Cmd:   []string{"/tools/by-builder", "--help"},
		},
		&container.HostConfig{
			Binds: []string{hostBin + ":/tools/by-builder:ro"},
		},
		containerName,
	)

	if err != nil {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("failed to run by-builder check: %v", err),
			Category: category,
		}
	}
	if exitCode != 0 {
		detail := stdout
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message: fmt.Sprintf("by-builder binary failed (exit %d); "+
				"check architecture matches the container image: %s", exitCode, detail),
			Category: category,
		}
	}

	return preflight.Result{
		Name:     name,
		Severity: preflight.SeverityOK,
		Message:  "by-builder binary is executable and correct architecture",
		Category: category,
	}
}

// checkMetadataBlocking probes whether the server can block container
// access to the cloud metadata endpoint (169.254.169.254). This
// requires either CAP_NET_ADMIN or an existing host-level iptables
// rule.
func checkMetadataBlocking(serverID string) preflight.Result {
	const name = "metadata_endpoint"
	const category = "docker"

	// Check if a blanket rule already exists.
	if DockerUserBlocksMetadata() {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityOK,
			Message:  "cloud metadata endpoint is blocked by host iptables rule",
			Category: category,
		}
	}

	// In container mode, also try a TCP connect test: if the metadata
	// endpoint is unreachable, it is already blocked.
	if serverID != "" {
		conn, err := net.DialTimeout("tcp", "169.254.169.254:80", 2*time.Second)
		if err != nil {
			return preflight.Result{
				Name:     name,
				Severity: preflight.SeverityOK,
				Message:  "cloud metadata endpoint is unreachable",
				Category: category,
			}
		}
		conn.Close()
	}

	// Probe whether we can manipulate iptables at all.
	if err := exec.Command("iptables", "-S", "DOCKER-USER").Run(); err != nil {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityWarning,
			Message: "cannot block cloud metadata endpoint (169.254.169.254); " +
				"grant CAP_NET_ADMIN to the server container, or add a host-level rule: " +
				"iptables -I DOCKER-USER -d 169.254.169.254/32 -j DROP",
			Category: category,
		}
	}

	return preflight.Result{
		Name:     name,
		Severity: preflight.SeverityOK,
		Message:  "iptables available for metadata endpoint blocking",
		Category: category,
	}
}

// checkRedisOnServiceNetwork verifies that the Redis server is NOT
// reachable on the Docker service network. If it is, worker containers
// can reach Redis, which breaks network isolation.
func checkRedisOnServiceNetwork(ctx context.Context, d *DockerBackend, deps PreflightDeps) preflight.Result {
	const name = "redis_on_service_network"
	const category = "docker"

	if deps.RedisURL == "" || d.config.ServiceNetwork == "" {
		return preflight.Result{Name: name, Severity: preflight.SeverityOK, Message: "Redis or service network not configured", Category: category}
	}

	u, err := url.Parse(deps.RedisURL)
	if err != nil {
		return preflight.Result{Name: name, Severity: preflight.SeverityOK, Message: "Redis URL check skipped (parse error)", Category: category}
	}
	redisHost := u.Hostname()
	if redisHost == "" {
		return preflight.Result{Name: name, Severity: preflight.SeverityOK, Message: "Redis URL check skipped (no host)", Category: category}
	}

	netResult, err := d.Client().NetworkInspect(ctx, d.config.ServiceNetwork, client.NetworkInspectOptions{})
	if err != nil {
		return preflight.Result{Name: name, Severity: preflight.SeverityOK, Message: "service network not yet available", Category: category}
	}

	for _, ep := range netResult.Network.Containers {
		if ep.Name == redisHost {
			return preflight.Result{
				Name:     name,
				Severity: preflight.SeverityError,
				Message: fmt.Sprintf("redis host %q is a container on the service network %q; "+
					"worker containers can reach it — move Redis to a separate network",
					redisHost, d.config.ServiceNetwork),
				Category: category,
			}
		}
		if ep.IPv4Address.IsValid() && ep.IPv4Address.Addr().String() == redisHost {
			return preflight.Result{
				Name:     name,
				Severity: preflight.SeverityError,
				Message: fmt.Sprintf("redis address %q matches a container on the service network %q; "+
					"worker containers can reach it — move Redis to a separate network",
					redisHost, d.config.ServiceNetwork),
				Category: category,
			}
		}
	}

	return preflight.Result{Name: name, Severity: preflight.SeverityOK, Message: "Redis is not on the service network", Category: category}
}

// --- helpers ---

// ensurePreflightImage pulls a Docker image if it is not already present
// locally. Renamed from `ensureImage` to avoid shadowing the
// (*DockerBackend).ensureImage method that lives in docker.go.
func ensurePreflightImage(ctx context.Context, cli *client.Client, img string) error {
	_, err := cli.ImageInspect(ctx, img)
	if err == nil {
		return nil
	}

	slog.Info("preflight: pulling image", "image", img)
	auth, err := dockerauth.RegistryAuthFor(img)
	if err != nil {
		slog.Warn("preflight: registry auth lookup failed, pulling anonymously", "image", img, "error", err)
	}
	pullResp, err := cli.ImagePull(ctx, img, client.ImagePullOptions{RegistryAuth: auth})
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
