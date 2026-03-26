package docker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
)

// Compile-time interface check.
var _ backend.Backend = (*DockerBackend)(nil)

// workerState holds per-worker internal state that callers never see.
// The Backend interface deals only in string worker IDs; this struct
// tracks what Docker resources are associated with each.
type workerState struct {
	containerID string
	networkID   string
	networkName string
}

// metadataMode caches the result of the first metadata endpoint check.
type metadataMode int

const (
	metadataUnchecked   metadataMode = iota
	metadataBlocked                  // iptables rule inserted successfully
	metadataHostManaged              // operator-installed blanket rule; skip per-network rules
)

// DockerBackend implements backend.Backend using the Docker Engine API.
type DockerBackend struct {
	client   *client.Client
	serverID string // own container ID; empty = native mode
	config   *config.DockerConfig
	mountCfg MountConfig

	mu      sync.Mutex
	workers map[string]*workerState // keyed by worker ID

	metaMu   sync.Mutex
	metaMode metadataMode
}

// New creates a DockerBackend, verifying Docker connectivity, detecting
// whether the server is running inside a container, and auto-detecting
// how the data directory is mounted.
func New(ctx context.Context, cfg *config.DockerConfig, bundleServerPath string) (*DockerBackend, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+cfg.Socket),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	if _, err := cli.Ping(ctx); err != nil {
		return nil, fmt.Errorf("docker ping: %w", err)
	}

	serverID := detectServerID()
	if serverID != "" {
		slog.Info("running in container mode", "server_id", serverID)
	} else {
		slog.Info("running in native mode (no server container ID detected)")
	}

	mountCfg, err := detectMountMode(ctx, cli, serverID, bundleServerPath)
	if err != nil {
		return nil, err
	}

	return &DockerBackend{
		client:   cli,
		serverID: serverID,
		config:   cfg,
		mountCfg: mountCfg,
		workers:  make(map[string]*workerState),
	}, nil
}

// --- Server ID detection ---

func detectServerID() string {
	// 1. Explicit env var
	if id := os.Getenv("BLOCKYARD_SERVER_ID"); id != "" {
		slog.Info("server ID from env", "container_id", id)
		return id
	}

	// 2. Parse /proc/self/cgroup
	if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if id := extractContainerIDFromCgroup(line); id != "" {
				slog.Info("server ID from cgroup", "container_id", id)
				return id
			}
		}
	}

	// 3. Hostname (Docker sets this to the short container ID)
	if data, err := os.ReadFile("/etc/hostname"); err == nil {
		hostname := strings.TrimSpace(string(data))
		if len(hostname) >= 12 && isHex(hostname) {
			slog.Info("server ID from hostname", "container_id", hostname)
			return hostname
		}
	}

	return ""
}

func extractContainerIDFromCgroup(line string) string {
	parts := strings.Split(line, "/")
	for i, part := range parts {
		switch {
		case part == "docker" && i+1 < len(parts):
			candidate := parts[i+1]
			if len(candidate) >= 12 && isHex(candidate) {
				return candidate
			}
		case strings.HasPrefix(part, "docker-") && strings.HasSuffix(part, ".scope"):
			candidate := strings.TrimPrefix(part, "docker-")
			candidate = strings.TrimSuffix(candidate, ".scope")
			if len(candidate) >= 12 && isHex(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// --- Label helpers ---

func workerLabels(spec backend.WorkerSpec) map[string]string {
	labels := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    spec.AppID,
		"dev.blockyard/worker-id": spec.WorkerID,
		"dev.blockyard/role":      "worker",
	}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	return labels
}

func buildLabels(spec backend.BuildSpec) map[string]string {
	labels := map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    spec.AppID,
		"dev.blockyard/bundle-id": spec.BundleID,
		"dev.blockyard/role":      "build",
	}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	return labels
}

func networkLabels(appID, workerID string) map[string]string {
	return map[string]string{
		"dev.blockyard/managed":   "true",
		"dev.blockyard/app-id":    appID,
		"dev.blockyard/worker-id": workerID,
	}
}

// --- Image pulling ---

func (d *DockerBackend) ensureImage(ctx context.Context, img string) error {
	_, err := d.client.ImageInspect(ctx, img)
	if err == nil {
		return nil // already present
	}

	slog.Info("pulling image", "image", img)
	reader, err := d.client.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", img, err)
	}
	defer reader.Close()

	// Must consume the reader for the pull to complete.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("pull image %s: %w", img, err)
	}

	slog.Info("image pulled", "image", img)
	return nil
}

// --- Memory limit parsing ---

// ParseMemoryLimit converts human-readable memory strings like "512m", "1g",
// "256mb" to bytes. Returns (bytes, true) on success.
func ParseMemoryLimit(s string) (int64, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	var numStr string
	var multiplier int64

	switch {
	case strings.HasSuffix(s, "gb"):
		numStr = strings.TrimSuffix(s, "gb")
		multiplier = 1024 * 1024 * 1024
	case strings.HasSuffix(s, "g"):
		numStr = strings.TrimSuffix(s, "g")
		multiplier = 1024 * 1024 * 1024
	case strings.HasSuffix(s, "mb"):
		numStr = strings.TrimSuffix(s, "mb")
		multiplier = 1024 * 1024
	case strings.HasSuffix(s, "m"):
		numStr = strings.TrimSuffix(s, "m")
		multiplier = 1024 * 1024
	case strings.HasSuffix(s, "kb"):
		numStr = strings.TrimSuffix(s, "kb")
		multiplier = 1024
	case strings.HasSuffix(s, "k"):
		numStr = strings.TrimSuffix(s, "k")
		multiplier = 1024
	default:
		numStr = s
		multiplier = 1 // assume bytes
	}

	n, err := strconv.ParseInt(strings.TrimSpace(numStr), 10, 64)
	if err != nil {
		return 0, false
	}
	return n * multiplier, true
}

// --- Network helpers ---

func (d *DockerBackend) createNetwork(
	ctx context.Context,
	name, appID, workerID string,
) (string, error) {
	resp, err := d.client.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
		Labels: networkLabels(appID, workerID),
	})
	if err != nil {
		return "", fmt.Errorf("create network %s: %w", name, err)
	}
	return resp.ID, nil
}

func (d *DockerBackend) joinNetwork(ctx context.Context, containerID, networkName string, aliases []string) error {
	var epConfig *network.EndpointSettings
	if len(aliases) > 0 {
		epConfig = &network.EndpointSettings{Aliases: aliases}
	}
	return d.client.NetworkConnect(ctx, networkName, containerID, epConfig)
}

func (d *DockerBackend) disconnectNetwork(ctx context.Context, containerID, networkName string) error {
	return d.client.NetworkDisconnect(ctx, networkName, containerID, true)
}

// connectServiceContainers inspects the configured service network and
// connects each container on it to the worker's per-worker network,
// preserving DNS aliases so the worker can resolve service hostnames.
func (d *DockerBackend) connectServiceContainers(ctx context.Context, workerNetworkName string) error {
	svcNet := d.config.ServiceNetwork
	if svcNet == "" {
		return nil
	}

	info, err := d.client.NetworkInspect(ctx, svcNet, network.InspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect service network %s: %w", svcNet, err)
	}

	for containerID := range info.Containers {
		if containerID == d.serverID {
			continue // server joins with "blockyard" alias separately
		}

		// Get DNS aliases from the service network endpoint.
		var aliases []string
		cInfo, err := d.client.ContainerInspect(ctx, containerID)
		if err != nil {
			slog.Warn("service network: cannot inspect container, skipping",
				"container_id", containerID, "error", err)
			continue
		}
		if ep, ok := cInfo.NetworkSettings.Networks[svcNet]; ok && ep != nil {
			aliases = ep.Aliases
		}

		if err := d.joinNetwork(ctx, containerID, workerNetworkName, aliases); err != nil {
			slog.Warn("service network: failed to connect container",
				"container_id", containerID,
				"worker_network", workerNetworkName, "error", err)
		} else {
			slog.Debug("service network: connected container",
				"container_id", containerID,
				"worker_network", workerNetworkName, "aliases", aliases)
		}
	}

	return nil
}

// disconnectServiceContainers removes service containers from the worker's
// network before the network is deleted.
func (d *DockerBackend) disconnectServiceContainers(ctx context.Context, workerNetworkName string) {
	svcNet := d.config.ServiceNetwork
	if svcNet == "" {
		return
	}

	info, err := d.client.NetworkInspect(ctx, svcNet, network.InspectOptions{})
	if err != nil {
		slog.Warn("service network: cannot inspect for disconnect",
			"service_network", svcNet, "error", err)
		return
	}

	for containerID := range info.Containers {
		if containerID == d.serverID {
			continue
		}
		if err := d.disconnectNetwork(ctx, containerID, workerNetworkName); err != nil {
			slog.Warn("service network: failed to disconnect container",
				"container_id", containerID,
				"worker_network", workerNetworkName, "error", err)
		}
	}
}

// --- Container creation ---

func (d *DockerBackend) createWorkerContainer(
	ctx context.Context,
	spec backend.WorkerSpec,
	networkName string,
) (string, error) {
	containerName := "blockyard-worker-" + spec.WorkerID

	binds, mounts := d.mountCfg.WorkerMounts(spec.BundlePath, spec.LibraryPath, spec.LibDir, spec.TransferDir, spec.TokenDir, spec.WorkerMount)

	// R_LIBS: use /blockyard-lib-store when store-assembled library is
	// available, else legacy /blockyard-lib. Must not use /lib as that
	// shadows the system shared library directory on Linux.
	rLibs := "/blockyard-lib"
	if spec.LibDir != "" {
		rLibs = "/blockyard-lib-store"
	}
	env := []string{
		fmt.Sprintf("SHINY_PORT=%d", spec.ShinyPort),
		"R_LIBS=" + rLibs,
	}
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}

	var resources container.Resources
	if spec.MemoryLimit != "" {
		if mem, ok := ParseMemoryLimit(spec.MemoryLimit); ok {
			resources.Memory = mem
		}
	}
	if spec.CPULimit > 0 {
		resources.NanoCPUs = int64(spec.CPULimit * 1e9)
	}
	resources.PidsLimit = int64Ptr(512)

	resp, err := d.client.ContainerCreate(ctx,
		&container.Config{
			Image:  spec.Image,
			Cmd:    spec.Cmd,
			Env:    env,
			Labels: workerLabels(spec),
		},
		&container.HostConfig{
			NetworkMode:    container.NetworkMode(networkName),
			Binds:          binds,
			Mounts:         mounts,
			Tmpfs:          map[string]string{"/tmp": ""},
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			Resources:      resources,
		},
		nil, nil,
		containerName,
	)
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", containerName, err)
	}

	return resp.ID, nil
}

// --- Log stream demuxing ---

func demuxReader(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, r)
		pw.CloseWithError(err)
	}()
	return pr
}

// --- Backend interface implementation ---

func (d *DockerBackend) Spawn(ctx context.Context, spec backend.WorkerSpec) error {
	spawnStart := time.Now()

	// 1. Ensure image exists locally
	slog.Debug("spawn: ensuring image", "worker_id", spec.WorkerID, "image", spec.Image)
	if err := d.ensureImage(ctx, spec.Image); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}

	networkName := "blockyard-" + spec.WorkerID

	// 2. Create per-worker bridge network
	slog.Debug("spawn: creating network", "worker_id", spec.WorkerID, "network", networkName)
	networkID, err := d.createNetwork(ctx, networkName, spec.AppID, spec.WorkerID)
	if err != nil {
		return fmt.Errorf("spawn: %w", err)
	}

	// 3. Block metadata endpoint (iptables)
	slog.Debug("spawn: blocking metadata endpoint", "worker_id", spec.WorkerID)
	if err := d.blockMetadataEndpoint(ctx, networkName, spec.WorkerID); err != nil {
		_ = d.client.NetworkRemove(ctx, networkID)
		return fmt.Errorf("spawn: metadata block: %w", err)
	}

	// 4. Create container
	slog.Debug("spawn: creating container", "worker_id", spec.WorkerID,
		"memory_limit", spec.MemoryLimit, "cpu_limit", spec.CPULimit)
	containerID, err := d.createWorkerContainer(ctx, spec, networkName)
	if err != nil {
		d.unblockMetadataForWorker(spec.WorkerID)
		_ = d.client.NetworkRemove(ctx, networkID)
		return fmt.Errorf("spawn: %w", err)
	}

	// 5. Connect service containers to worker network
	if d.config.ServiceNetwork != "" {
		slog.Debug("spawn: connecting service containers",
			"worker_id", spec.WorkerID, "service_network", d.config.ServiceNetwork)
		if err := d.connectServiceContainers(ctx, networkName); err != nil {
			_ = d.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
			d.unblockMetadataForWorker(spec.WorkerID)
			_ = d.client.NetworkRemove(ctx, networkID)
			return fmt.Errorf("spawn: %w", err)
		}
	}

	// 6. Join server to worker network (if running in a container)
	if d.serverID != "" {
		slog.Debug("spawn: joining server to worker network",
			"worker_id", spec.WorkerID, "server_id", d.serverID)
		if err := d.joinNetwork(ctx, d.serverID, networkName, []string{"blockyard"}); err != nil {
			d.disconnectServiceContainers(ctx, networkName)
			_ = d.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
			d.unblockMetadataForWorker(spec.WorkerID)
			_ = d.client.NetworkRemove(ctx, networkID)
			return fmt.Errorf("spawn: %w", err)
		}
	}

	// 7. Start the container
	slog.Debug("spawn: starting container", "worker_id", spec.WorkerID, "container_id", containerID)
	if err := d.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		if d.serverID != "" {
			_ = d.disconnectNetwork(ctx, d.serverID, networkName)
		}
		d.disconnectServiceContainers(ctx, networkName)
		_ = d.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		d.unblockMetadataForWorker(spec.WorkerID)
		_ = d.client.NetworkRemove(ctx, networkID)
		return fmt.Errorf("spawn: start container: %w", err)
	}

	// Verify resource limits match what was requested.
	d.verifyResourceLimits(ctx, containerID, spec)

	// Record internal state
	d.mu.Lock()
	d.workers[spec.WorkerID] = &workerState{
		containerID: containerID,
		networkID:   networkID,
		networkName: networkName,
	}
	d.mu.Unlock()

	slog.Debug("spawn: container started",
		"worker_id", spec.WorkerID, "container_id", containerID,
		"elapsed_ms", time.Since(spawnStart).Milliseconds())

	return nil
}

// verifyResourceLimits inspects a running container and warns if
// actual resource limits don't match what was requested.
func (d *DockerBackend) verifyResourceLimits(
	ctx context.Context,
	containerID string,
	spec backend.WorkerSpec,
) {
	info, err := d.client.ContainerInspect(ctx, containerID)
	if err != nil {
		slog.Warn("spawn: failed to verify resource limits",
			"worker_id", spec.WorkerID, "error", err)
		return
	}

	if spec.MemoryLimit != "" {
		expected, ok := ParseMemoryLimit(spec.MemoryLimit)
		if ok && info.HostConfig.Resources.Memory != expected {
			slog.Warn("spawn: memory limit mismatch",
				"worker_id", spec.WorkerID,
				"requested", spec.MemoryLimit,
				"expected_bytes", expected,
				"actual_bytes", info.HostConfig.Resources.Memory)
		}
	}

	if spec.CPULimit > 0 {
		expected := int64(spec.CPULimit * 1e9)
		if info.HostConfig.Resources.NanoCPUs != expected {
			slog.Warn("spawn: CPU limit mismatch",
				"worker_id", spec.WorkerID,
				"requested_cpus", spec.CPULimit,
				"expected_nanocpus", expected,
				"actual_nanocpus", info.HostConfig.Resources.NanoCPUs)
		}
	}
}

func (d *DockerBackend) Stop(ctx context.Context, id string) error {
	d.mu.Lock()
	ws, ok := d.workers[id]
	if ok {
		delete(d.workers, id)
	}
	d.mu.Unlock()

	if !ok {
		return fmt.Errorf("stop: unknown worker %s", id)
	}

	var firstErr error

	// 1. Stop the container (10s timeout)
	timeout := 10
	if err := d.client.ContainerStop(ctx, ws.containerID, container.StopOptions{
		Timeout: &timeout,
	}); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("stop container: %w", err)
		slog.Warn("failed to stop container", "worker_id", id, "error", err)
	}

	// 2. Remove the container
	if err := d.client.ContainerRemove(ctx, ws.containerID, container.RemoveOptions{
		Force: true,
	}); err != nil {
		slog.Warn("failed to remove container", "worker_id", id, "error", err)
	}

	// 3. Disconnect server from the worker's network
	if d.serverID != "" {
		if err := d.disconnectNetwork(ctx, d.serverID, ws.networkName); err != nil {
			slog.Warn("failed to disconnect from network", "worker_id", id, "error", err)
		}
	}

	// 4. Disconnect service containers from the worker's network
	d.disconnectServiceContainers(ctx, ws.networkName)

	// 5. Remove iptables metadata block rule
	d.unblockMetadataForWorker(id)

	// 6. Remove the network
	if err := d.client.NetworkRemove(ctx, ws.networkName); err != nil {
		slog.Warn("failed to remove network", "worker_id", id, "error", err)
	}

	return firstErr
}

func (d *DockerBackend) HealthCheck(ctx context.Context, id string) bool {
	addr, err := d.Addr(ctx, id)
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (d *DockerBackend) Logs(ctx context.Context, id string) (backend.LogStream, error) {
	d.mu.Lock()
	ws, ok := d.workers[id]
	d.mu.Unlock()
	if !ok {
		return backend.LogStream{}, fmt.Errorf("logs: unknown worker %s", id)
	}

	reader, err := d.client.ContainerLogs(ctx, ws.containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return backend.LogStream{}, fmt.Errorf("logs: %w", err)
	}

	lines := make(chan string, 256)
	logCtx, logCancel := context.WithCancel(ctx)

	go func() {
		defer close(lines)
		defer reader.Close()

		scanner := bufio.NewScanner(demuxReader(reader))
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-logCtx.Done():
				return
			}
		}
	}()

	return backend.LogStream{
		Lines: lines,
		Close: logCancel,
	}, nil
}

func (d *DockerBackend) Addr(ctx context.Context, id string) (string, error) {
	d.mu.Lock()
	ws, ok := d.workers[id]
	d.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("addr: unknown worker %s", id)
	}

	info, err := d.client.ContainerInspect(ctx, ws.containerID)
	if err != nil {
		return "", fmt.Errorf("addr: inspect container: %w", err)
	}

	if info.NetworkSettings == nil || info.NetworkSettings.Networks == nil {
		return "", fmt.Errorf("addr: no networks on container %s", id)
	}

	endpoint, ok := info.NetworkSettings.Networks[ws.networkName]
	if !ok {
		return "", fmt.Errorf("addr: container not on network %s", ws.networkName)
	}

	if endpoint.IPAddress == "" {
		return "", fmt.Errorf("addr: no IP on network %s", ws.networkName)
	}

	return fmt.Sprintf("%s:%d", endpoint.IPAddress, d.config.ShinyPort), nil
}

func (d *DockerBackend) Build(ctx context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
	// 1. Ensure image exists locally
	if err := d.ensureImage(ctx, spec.Image); err != nil {
		return backend.BuildResult{}, fmt.Errorf("build: %w", err)
	}

	// 2. Create isolated build network with metadata blocking
	buildNetworkName := "blockyard-build-" + spec.BundleID
	_, err := d.createNetwork(ctx, buildNetworkName, spec.AppID, spec.BundleID)
	if err != nil {
		return backend.BuildResult{}, fmt.Errorf("build: %w", err)
	}
	defer func() {
		d.client.NetworkRemove(ctx, buildNetworkName) //nolint:errcheck // best-effort cleanup
	}()

	if err := d.blockMetadataEndpoint(ctx, buildNetworkName, "build-"+spec.BundleID); err != nil {
		return backend.BuildResult{}, fmt.Errorf("build: metadata block: %w", err)
	}
	defer func() {
		d.unblockMetadataForWorker("build-" + spec.BundleID)
	}()

	containerName := "blockyard-build-" + spec.BundleID

	// 3. Create container
	var binds []string
	var mounts []mount.Mount
	for _, m := range spec.Mounts {
		b, dm := d.mountCfg.TranslateMount(m)
		binds = append(binds, b...)
		mounts = append(mounts, dm...)
	}

	resp, err := d.client.ContainerCreate(ctx,
		&container.Config{
			Image:      spec.Image,
			Cmd:        spec.Cmd,
			WorkingDir: "/app",
			Labels:     buildLabels(spec),
			Env:        spec.Env,
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(buildNetworkName),
			Binds:       binds,
			Mounts:      mounts,
			Tmpfs: map[string]string{
				"/tmp": "exec",
			},
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Resources: container.Resources{
				PidsLimit: int64Ptr(512),
			},
		},
		nil, nil,
		containerName,
	)
	if err != nil {
		return backend.BuildResult{}, fmt.Errorf("build: create container: %w", err)
	}

	containerID := resp.ID

	// 4. Start the build container
	if err := d.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		_ = d.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		return backend.BuildResult{}, fmt.Errorf("build: start container: %w", err)
	}

	// 5. Stream logs in real-time while the build runs.
	var buildLogs strings.Builder
	if logReader, logErr := d.client.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	}); logErr == nil {
		scanner := bufio.NewScanner(demuxReader(logReader))
		for scanner.Scan() {
			line := scanner.Text()
			buildLogs.WriteString(line)
			buildLogs.WriteByte('\n')
			if spec.LogWriter != nil {
				spec.LogWriter(line)
			}
		}
		logReader.Close()
	}

	// 6. Wait for exit (container has already stopped since log follow ended).
	waitCh, errCh := d.client.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)

	var exitCode int
	select {
	case result := <-waitCh:
		exitCode = int(result.StatusCode)
	case err := <-errCh:
		slog.Warn("build container wait error", "error", err)
		exitCode = -1
	case <-ctx.Done():
		slog.Warn("build cancelled", "bundle_id", spec.BundleID)
		exitCode = -1
	}

	success := exitCode == 0

	// 7. Remove the build container
	_ = d.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})

	return backend.BuildResult{
		Success:  success,
		ExitCode: exitCode,
		Logs:     buildLogs.String(),
	}, nil
}

func (d *DockerBackend) ListManaged(ctx context.Context) ([]backend.ManagedResource, error) {
	var resources []backend.ManagedResource

	// Find managed containers (including stopped)
	containers, err := d.client.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", "dev.blockyard/managed=true"),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("list managed containers: %w", err)
	}
	for _, c := range containers {
		resources = append(resources, backend.ManagedResource{
			ID:     c.ID,
			Kind:   backend.ResourceContainer,
			Labels: c.Labels,
		})
	}

	// Find managed networks
	networks, err := d.client.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", "dev.blockyard/managed=true"),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("list managed networks: %w", err)
	}
	for _, n := range networks {
		resources = append(resources, backend.ManagedResource{
			ID:     n.ID,
			Kind:   backend.ResourceNetwork,
			Labels: n.Labels,
		})
	}

	// Containers first so they're removed before their networks
	sort.Slice(resources, func(i, j int) bool {
		return resources[i].Kind < resources[j].Kind
	})

	return resources, nil
}

func (d *DockerBackend) RemoveResource(ctx context.Context, r backend.ManagedResource) error {
	switch r.Kind {
	case backend.ResourceContainer:
		return d.client.ContainerRemove(ctx, r.ID, container.RemoveOptions{Force: true})
	case backend.ResourceNetwork:
		return d.client.NetworkRemove(ctx, r.ID)
	default:
		return fmt.Errorf("unknown resource kind: %d", r.Kind)
	}
}

// --- Metadata endpoint protection ---

// blockMetadataEndpoint blocks container access to the cloud metadata endpoint
// (169.254.169.254) using iptables rules scoped to the worker network's subnet.
func (d *DockerBackend) blockMetadataEndpoint(ctx context.Context, networkName, workerID string) error {
	d.metaMu.Lock()
	defer d.metaMu.Unlock()

	switch d.metaMode {
	case metadataBlocked:
		// iptables works — insert a rule for this worker
		return d.insertMetadataRule(ctx, networkName, workerID)
	case metadataHostManaged:
		// Already blocked by operator rule — nothing to do
		return nil
	}

	// First spawn — detect capabilities

	// Inspect network to get subnet CIDR
	info, err := d.client.NetworkInspect(ctx, networkName, network.InspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect network for metadata rule: %w", err)
	}
	if len(info.IPAM.Config) == 0 {
		return fmt.Errorf("network %s has no IPAM config", networkName)
	}
	subnet := info.IPAM.Config[0].Subnet

	// Try inserting iptables rule
	comment := "blockyard-" + workerID
	insertErr := exec.CommandContext(ctx, "iptables",
		"-I", "DOCKER-USER",
		"-s", subnet,
		"-d", "169.254.169.254/32",
		"-j", "DROP",
		"-m", "comment", "--comment", comment,
	).Run()

	if insertErr == nil {
		slog.Debug("metadata endpoint blocked", "worker_id", workerID, "subnet", subnet)
		d.metaMode = metadataBlocked
		return nil
	}

	// iptables failed — check if a host-level rule already blocks the endpoint
	if d.hostBlocksMetadataEndpoint() {
		slog.Info("metadata endpoint blocked by host-level rule")
		d.metaMode = metadataHostManaged
		return nil
	}

	return fmt.Errorf(
		"cannot block metadata endpoint: grant CAP_NET_ADMIN to the " +
			"server container, or add a host-level iptables rule: " +
			"iptables -I DOCKER-USER -d 169.254.169.254/32 -j DROP",
	)
}

// hostBlocksMetadataEndpoint checks whether the metadata endpoint is already
// blocked for Docker containers.
//
// In native mode (no serverID), a TCP connect from the host does NOT reflect
// whether Docker containers are blocked — it tests host networking, not
// Docker-forwarded traffic. Instead we check the iptables DOCKER-USER chain
// for an existing DROP rule targeting 169.254.169.254.
//
// In container mode (serverID set), the server shares Docker networking, so a
// TCP connect is a valid proxy for container reachability.
func (d *DockerBackend) hostBlocksMetadataEndpoint() bool {
	if d.serverID == "" {
		// Native mode: check iptables DOCKER-USER chain directly
		return dockerUserBlocksMetadata()
	}

	// Container mode: TCP connect test
	conn, err := net.DialTimeout("tcp", "169.254.169.254:80", 2*time.Second)
	if err != nil {
		return true // unreachable = blocked
	}
	conn.Close()
	return false // reachable = not blocked
}

// dockerUserBlocksMetadata checks if the DOCKER-USER iptables chain contains a
// DROP rule for 169.254.169.254. Tries both direct iptables and sudo iptables.
func dockerUserBlocksMetadata() bool {
	for _, args := range [][]string{
		{"iptables", "-S", "DOCKER-USER"},
		{"sudo", "iptables", "-S", "DOCKER-USER"},
	} {
		out, err := exec.Command(args[0], args[1:]...).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "169.254.169.254") && strings.Contains(line, "DROP") {
				return true
			}
		}
	}
	return false
}

func (d *DockerBackend) insertMetadataRule(ctx context.Context, networkName, workerID string) error {
	info, err := d.client.NetworkInspect(ctx, networkName, network.InspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect network for metadata rule: %w", err)
	}
	if len(info.IPAM.Config) == 0 {
		return fmt.Errorf("network %s has no IPAM config", networkName)
	}
	subnet := info.IPAM.Config[0].Subnet

	comment := "blockyard-" + workerID
	if err := exec.CommandContext(ctx, "iptables",
		"-I", "DOCKER-USER",
		"-s", subnet,
		"-d", "169.254.169.254/32",
		"-j", "DROP",
		"-m", "comment", "--comment", comment,
	).Run(); err != nil {
		return fmt.Errorf("insert iptables rule: %w", err)
	}

	return nil
}

// unblockMetadataForWorker removes the iptables rule for a specific worker.
func (d *DockerBackend) unblockMetadataForWorker(workerID string) {
	d.metaMu.Lock()
	mode := d.metaMode
	d.metaMu.Unlock()

	if mode != metadataBlocked {
		return
	}
	comment := "blockyard-" + workerID
	deleteIptablesRulesByComment(comment)
}

// deleteIptablesRulesByComment uses `iptables -S DOCKER-USER` to find rules
// containing the given comment string, then deletes them with `iptables -D`.
// The comment must match as a complete --comment argument (quoted in the -S
// output), not as an arbitrary substring.
func deleteIptablesRulesByComment(comment string) {
	out, err := exec.Command("iptables", "-S", "DOCKER-USER").Output()
	if err != nil {
		return
	}

	// Match the exact comment flag as it appears in iptables -S output.
	needle := `--comment "` + comment + `"`
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		rule := strings.TrimPrefix(line, "-A DOCKER-USER ")
		if rule == line {
			continue // didn't have the expected prefix
		}
		args := append([]string{"-D", "DOCKER-USER"}, strings.Fields(rule)...)
		if err := exec.Command("iptables", args...).Run(); err != nil {
			slog.Warn("failed to delete iptables rule", "comment", comment, "error", err)
		}
	}
}

// CleanupOrphanMetadataRules removes all blockyard iptables rules left over
// from previous runs. Called at server startup.
func CleanupOrphanMetadataRules() {
	deleteIptablesRulesByComment("blockyard-")
}

func int64Ptr(v int64) *int64 { return &v }
