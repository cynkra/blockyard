package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/units"
)

// Compile-time interface check.
var _ backend.Backend = (*DockerBackend)(nil)

// dockerClient abstracts the Docker API methods used by DockerBackend,
// enabling unit tests to supply a mock instead of a real daemon connection.
// The concrete *client.Client satisfies this interface.
type dockerClient interface {
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStats(ctx context.Context, containerID string, options client.ContainerStatsOptions) (client.ContainerStatsResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerUpdate(ctx context.Context, containerID string, options client.ContainerUpdateOptions) (client.ContainerUpdateResult, error)
	ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult
	ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(ctx context.Context, refStr string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	NetworkConnect(ctx context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error)
	NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	NetworkDisconnect(ctx context.Context, networkID string, options client.NetworkDisconnectOptions) (client.NetworkDisconnectResult, error)
	NetworkInspect(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	NetworkList(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
}

// cmdRunner executes a system command and returns its combined output.
// The default uses exec.CommandContext; tests can substitute a mock.
type cmdRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func defaultCmdRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // G204: controlled docker exec calls
}

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
	client           dockerClient
	serverID         string // own container ID; empty = native mode
	config           *config.DockerConfig // shortcut for fullCfg.Docker
	fullCfg          *config.Config       // full config; needed for Server.DefaultMemoryLimit/CPULimit and the redis URL the preflight builder reads
	bundleServerPath string               // root for the by-builder cache and pkg-store
	serverVersion    string               // server build version, plumbed into preflight
	redisURL         string               // optional; used by checkRedisOnServiceNetwork
	mountCfg         MountConfig
	runCmd           cmdRunner

	mu      sync.Mutex
	workers map[string]*workerState // keyed by worker ID

	metaMu   sync.Mutex
	metaMode metadataMode
}

// New creates a DockerBackend, verifying Docker connectivity, detecting
// whether the server is running inside a container, and auto-detecting
// how the data directory is mounted.
//
// Takes the full *config.Config (parallel to process.New) so the
// backend can read both `[docker]` and the worker-resource-limit
// defaults that now live in `[server]`.
func New(ctx context.Context, fullCfg *config.Config, bundleServerPath, serverVersion string) (*DockerBackend, error) {
	cfg := &fullCfg.Docker
	cli, err := client.New(
		client.WithHost("unix://"+cfg.Socket),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
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

	var redisURL string
	if fullCfg.Redis != nil {
		redisURL = fullCfg.Redis.URL
	}

	return &DockerBackend{
		client:           cli,
		serverID:         serverID,
		config:           cfg,
		fullCfg:          fullCfg,
		bundleServerPath: bundleServerPath,
		serverVersion:    serverVersion,
		redisURL:         redisURL,
		mountCfg:         mountCfg,
		runCmd:           defaultCmdRunner,
		workers:          make(map[string]*workerState),
	}, nil
}

// Client returns the underlying Docker API client.
func (d *DockerBackend) Client() *client.Client { return d.client.(*client.Client) }

// MountCfg returns the auto-detected mount configuration.
func (d *DockerBackend) MountCfg() MountConfig { return d.mountCfg }

// ServerID returns the container ID of the server, or empty in native mode.
func (d *DockerBackend) ServerID() string { return d.serverID }

// --- Server ID detection ---

func detectServerID() string {
	// 1. Explicit env var
	if id := os.Getenv("BLOCKYARD_SERVER_ID"); id != "" {
		slog.Info("server ID from env", "container_id", id) //nolint:gosec // G706: slog structured logging handles this
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
	pullResp, err := d.client.ImagePull(ctx, img, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", img, err)
	}
	defer pullResp.Close()

	// Must consume the reader for the pull to complete.
	if _, err := io.Copy(io.Discard, pullResp); err != nil {
		return fmt.Errorf("pull image %s: %w", img, err)
	}

	slog.Info("image pulled", "image", img)
	return nil
}

// --- Network helpers ---

func (d *DockerBackend) createNetwork(
	ctx context.Context,
	name, appID, workerID string,
) (string, error) {
	resp, err := d.client.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: networkLabels(appID, workerID),
	})
	if err != nil {
		return "", fmt.Errorf("create network %s: %w", name, err)
	}
	return resp.ID, nil
}

func (d *DockerBackend) joinNetwork(ctx context.Context, containerID, networkName string, aliases []string) error {
	opts := client.NetworkConnectOptions{Container: containerID}
	if len(aliases) > 0 {
		opts.EndpointConfig = &network.EndpointSettings{Aliases: aliases}
	}
	_, err := d.client.NetworkConnect(ctx, networkName, opts)
	return err
}

func (d *DockerBackend) disconnectNetwork(ctx context.Context, containerID, networkName string) error {
	_, err := d.client.NetworkDisconnect(ctx, networkName, client.NetworkDisconnectOptions{
		Container: containerID,
		Force:     true,
	})
	return err
}

// connectServiceContainers inspects the configured service network and
// connects each container on it to the worker's per-worker network,
// preserving DNS aliases so the worker can resolve service hostnames.
func (d *DockerBackend) connectServiceContainers(ctx context.Context, workerNetworkName string) error {
	svcNet := d.config.ServiceNetwork
	if svcNet == "" {
		return nil
	}

	netResult, err := d.client.NetworkInspect(ctx, svcNet, client.NetworkInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect service network %s: %w", svcNet, err)
	}

	for containerID := range netResult.Network.Containers {
		if containerID == d.serverID {
			continue // server joins with "blockyard" alias separately
		}

		// Get DNS aliases from the service network endpoint.
		var aliases []string
		cResult, err := d.client.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		if err != nil {
			slog.Warn("service network: cannot inspect container, skipping",
				"container_id", containerID, "error", err)
			continue
		}
		if ep, ok := cResult.Container.NetworkSettings.Networks[svcNet]; ok && ep != nil {
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

	netResult, err := d.client.NetworkInspect(ctx, svcNet, client.NetworkInspectOptions{})
	if err != nil {
		slog.Warn("service network: cannot inspect for disconnect",
			"service_network", svcNet, "error", err)
		return
	}

	for containerID := range netResult.Network.Containers {
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

	// Append per-app data mounts. Source paths are host paths (not
	// server-container paths), so they bypass MountConfig translation
	// and go directly into Docker bind strings.
	for _, dm := range spec.DataMounts {
		flag := ":ro"
		if !dm.ReadOnly {
			flag = ""
		}
		binds = append(binds, dm.Source+":"+dm.Target+flag)
	}

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
		if mem, ok := units.ParseMemoryLimit(spec.MemoryLimit); ok {
			resources.Memory = mem
		}
	} else if d.fullCfg.Server.DefaultMemoryLimit != "" {
		if mem, ok := units.ParseMemoryLimit(d.fullCfg.Server.DefaultMemoryLimit); ok {
			resources.Memory = mem
		}
	}
	if spec.CPULimit > 0 {
		resources.NanoCPUs = int64(spec.CPULimit * 1e9)
	} else if d.fullCfg.Server.DefaultCPULimit > 0 {
		resources.NanoCPUs = int64(d.fullCfg.Server.DefaultCPULimit * 1e9)
	}
	resources.PidsLimit = int64Ptr(512)

	resp, err := d.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:  spec.Image,
			Cmd:    spec.Cmd,
			Env:    env,
			Labels: workerLabels(spec),
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(networkName),
			Binds:          binds,
			Mounts:         mounts,
			Tmpfs:          map[string]string{"/tmp": ""},
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			Resources:      resources,
			Runtime:        spec.Runtime,
		},
		Name: containerName,
	})
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", containerName, err)
	}

	return resp.ID, nil
}

// UpdateResources live-updates resource limits on a running container.
func (d *DockerBackend) UpdateResources(ctx context.Context, id string, mem int64, nanoCPUs int64) error {
	d.mu.Lock()
	ws, ok := d.workers[id]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("worker %s not found", id)
	}

	resources := container.Resources{}
	if mem > 0 {
		resources.Memory = mem
	}
	if nanoCPUs > 0 {
		resources.NanoCPUs = nanoCPUs
	}

	_, err := d.client.ContainerUpdate(ctx, ws.containerID,
		client.ContainerUpdateOptions{Resources: &resources})
	return err
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
		_, _ = d.client.NetworkRemove(ctx, networkID, client.NetworkRemoveOptions{})
		return fmt.Errorf("spawn: metadata block: %w", err)
	}

	// 4. Create container
	slog.Debug("spawn: creating container", "worker_id", spec.WorkerID,
		"memory_limit", spec.MemoryLimit, "cpu_limit", spec.CPULimit)
	containerID, err := d.createWorkerContainer(ctx, spec, networkName)
	if err != nil {
		d.unblockMetadataForWorker(spec.WorkerID)
		_, _ = d.client.NetworkRemove(ctx, networkID, client.NetworkRemoveOptions{})
		return fmt.Errorf("spawn: %w", err)
	}

	// 5. Connect service containers to worker network
	if d.config.ServiceNetwork != "" {
		slog.Debug("spawn: connecting service containers",
			"worker_id", spec.WorkerID, "service_network", d.config.ServiceNetwork)
		if err := d.connectServiceContainers(ctx, networkName); err != nil {
			_, _ = d.client.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
			d.unblockMetadataForWorker(spec.WorkerID)
			_, _ = d.client.NetworkRemove(ctx, networkID, client.NetworkRemoveOptions{})
			return fmt.Errorf("spawn: %w", err)
		}
	}

	// 6. Join server to worker network (if running in a container)
	if d.serverID != "" {
		slog.Debug("spawn: joining server to worker network",
			"worker_id", spec.WorkerID, "server_id", d.serverID)
		if err := d.joinNetwork(ctx, d.serverID, networkName, []string{"blockyard"}); err != nil {
			d.disconnectServiceContainers(ctx, networkName)
			_, _ = d.client.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
			d.unblockMetadataForWorker(spec.WorkerID)
			_, _ = d.client.NetworkRemove(ctx, networkID, client.NetworkRemoveOptions{})
			return fmt.Errorf("spawn: %w", err)
		}
	}

	// 7. Start the container
	slog.Debug("spawn: starting container", "worker_id", spec.WorkerID, "container_id", containerID)
	if _, err := d.client.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		if d.serverID != "" {
			_ = d.disconnectNetwork(ctx, d.serverID, networkName)
		}
		d.disconnectServiceContainers(ctx, networkName)
		_, _ = d.client.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
		d.unblockMetadataForWorker(spec.WorkerID)
		_, _ = d.client.NetworkRemove(ctx, networkID, client.NetworkRemoveOptions{})
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
	cResult, err := d.client.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		slog.Warn("spawn: failed to verify resource limits",
			"worker_id", spec.WorkerID, "error", err)
		return
	}
	info := cResult.Container

	if spec.MemoryLimit != "" {
		expected, ok := units.ParseMemoryLimit(spec.MemoryLimit)
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
	if _, err := d.client.ContainerStop(ctx, ws.containerID, client.ContainerStopOptions{
		Timeout: &timeout,
	}); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("stop container: %w", err)
		slog.Warn("failed to stop container", "worker_id", id, "error", err)
	}

	// 2. Remove the container
	if _, err := d.client.ContainerRemove(ctx, ws.containerID, client.ContainerRemoveOptions{
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
	if _, err := d.client.NetworkRemove(ctx, ws.networkName, client.NetworkRemoveOptions{}); err != nil {
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

	reader, err := d.client.ContainerLogs(ctx, ws.containerID, client.ContainerLogsOptions{
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

	cResult, err := d.client.ContainerInspect(ctx, ws.containerID, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("addr: inspect container: %w", err)
	}
	info := cResult.Container

	if info.NetworkSettings == nil || info.NetworkSettings.Networks == nil {
		return "", fmt.Errorf("addr: no networks on container %s", id)
	}

	endpoint, ok := info.NetworkSettings.Networks[ws.networkName]
	if !ok {
		return "", fmt.Errorf("addr: container not on network %s", ws.networkName)
	}

	if !endpoint.IPAddress.IsValid() {
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
		d.client.NetworkRemove(ctx, buildNetworkName, client.NetworkRemoveOptions{}) //nolint:errcheck // best-effort cleanup
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

	resp, err := d.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      spec.Image,
			Cmd:        spec.Cmd,
			WorkingDir: "/app",
			Labels:     buildLabels(spec),
			Env:        spec.Env,
		},
		HostConfig: &container.HostConfig{
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
		Name: containerName,
	})
	if err != nil {
		return backend.BuildResult{}, fmt.Errorf("build: create container: %w", err)
	}

	containerID := resp.ID

	// 4. Start the build container
	if _, err := d.client.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		_, _ = d.client.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
		return backend.BuildResult{}, fmt.Errorf("build: start container: %w", err)
	}

	// 5. Stream logs in real-time while the build runs.
	// Close the log reader when the context is cancelled so scanner.Scan()
	// unblocks instead of hanging forever on a stalled container.
	var buildLogs strings.Builder
	if logReader, logErr := d.client.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	}); logErr == nil {
		go func() {
			<-ctx.Done()
			logReader.Close()
		}()
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
	waitResult := d.client.ContainerWait(ctx, containerID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})

	var exitCode int
	select {
	case result := <-waitResult.Result:
		exitCode = int(result.StatusCode)
	case err := <-waitResult.Error:
		slog.Warn("build container wait error", "error", err)
		exitCode = -1
	case <-ctx.Done():
		slog.Warn("build cancelled", "bundle_id", spec.BundleID)
		exitCode = -1
	}

	success := exitCode == 0

	// 7. Remove the build container
	_, _ = d.client.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})

	return backend.BuildResult{
		Success:  success,
		ExitCode: exitCode,
		Logs:     buildLogs.String(),
	}, nil
}

func (d *DockerBackend) ListManaged(ctx context.Context) ([]backend.ManagedResource, error) {
	var resources []backend.ManagedResource

	// Find managed containers (including stopped)
	containerResult, err := d.client.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", "dev.blockyard/managed=true"),
	})
	if err != nil {
		return nil, fmt.Errorf("list managed containers: %w", err)
	}
	for _, c := range containerResult.Items {
		resources = append(resources, backend.ManagedResource{
			ID:     c.ID,
			Kind:   backend.ResourceContainer,
			Labels: c.Labels,
		})
	}

	// Find managed networks
	networkResult, err := d.client.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("label", "dev.blockyard/managed=true"),
	})
	if err != nil {
		return nil, fmt.Errorf("list managed networks: %w", err)
	}
	for _, n := range networkResult.Items {
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

func (d *DockerBackend) WorkerResourceUsage(ctx context.Context, workerID string) (*backend.WorkerResourceUsageResult, error) {
	d.mu.Lock()
	ws, ok := d.workers[workerID]
	d.mu.Unlock()
	containerID := workerID
	if ok {
		containerID = ws.containerID
	}

	resp, err := d.client.ContainerStats(ctx, containerID, client.ContainerStatsOptions{
		IncludePreviousSample: true,
	})
	if err != nil {
		return nil, fmt.Errorf("worker resource usage: %w", err)
	}
	defer resp.Body.Close()

	// Docker stats API returns a JSON stream; read one frame.
	var statsJSON struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs     uint64 `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage uint64 `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
		MemoryStats struct {
			Usage uint64 `json:"usage"`
			Limit uint64 `json:"limit"`
		} `json:"memory_stats"`
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read stats: %w", err)
	}

	if err := json.Unmarshal(data, &statsJSON); err != nil {
		return nil, fmt.Errorf("decode stats: %w", err)
	}

	// Calculate CPU percentage
	cpuDelta := float64(statsJSON.CPUStats.CPUUsage.TotalUsage - statsJSON.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(statsJSON.CPUStats.SystemCPUUsage - statsJSON.PreCPUStats.SystemCPUUsage)
	cpuPercent := 0.0
	if sysDelta > 0 && cpuDelta > 0 {
		cpuPercent = (cpuDelta / sysDelta) * float64(statsJSON.CPUStats.OnlineCPUs) * 100.0
	}

	return &backend.WorkerResourceUsageResult{
		CPUPercent:       cpuPercent,
		MemoryUsageBytes: statsJSON.MemoryStats.Usage,
		MemoryLimitBytes: statsJSON.MemoryStats.Limit,
	}, nil
}

func (d *DockerBackend) RemoveResource(ctx context.Context, r backend.ManagedResource) error {
	switch r.Kind {
	case backend.ResourceContainer:
		_, err := d.client.ContainerRemove(ctx, r.ID, client.ContainerRemoveOptions{Force: true})
		return err
	case backend.ResourceNetwork:
		_, err := d.client.NetworkRemove(ctx, r.ID, client.NetworkRemoveOptions{})
		return err
	default:
		return fmt.Errorf("unknown resource kind: %d", r.Kind)
	}
}

// --- Metadata endpoint protection ---

// validIptablesComment checks that s is safe to use as an iptables --comment
// value. Only alphanumerics, hyphens, and underscores are allowed.
func validIptablesComment(s string) bool {
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return len(s) > 0
}

// blockMetadataEndpoint blocks container access to the cloud metadata endpoint
// (169.254.169.254) using iptables rules scoped to the worker network's subnet.
func (d *DockerBackend) blockMetadataEndpoint(ctx context.Context, networkName, workerID string) error {
	if !validIptablesComment(workerID) {
		return fmt.Errorf("invalid worker ID for iptables comment: %q", workerID)
	}

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
	netResult, err := d.client.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect network for metadata rule: %w", err)
	}
	if len(netResult.Network.IPAM.Config) == 0 {
		return fmt.Errorf("network %s has no IPAM config", networkName)
	}
	subnet := netResult.Network.IPAM.Config[0].Subnet

	// Try inserting iptables rule
	comment := "blockyard-" + workerID
	_, insertErr := d.runCmd(ctx, "iptables",
		"-I", "DOCKER-USER",
		"-s", subnet.String(),
		"-d", "169.254.169.254/32",
		"-j", "DROP",
		"-m", "comment", "--comment", comment,
	)

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
		return d.dockerUserBlocksMetadata()
	}

	// Container mode: TCP connect test
	conn, err := net.DialTimeout("tcp", "169.254.169.254:80", 2*time.Second)
	if err != nil {
		return true // unreachable = blocked
	}
	conn.Close()
	return false // reachable = not blocked
}

// DockerUserBlocksMetadata checks if the DOCKER-USER iptables chain contains a
// DROP rule for 169.254.169.254. Exported for use by preflight checks that run
// before a DockerBackend exists.
func DockerUserBlocksMetadata() bool {
	return dockerUserBlocksMetadataWithRunner(defaultCmdRunner)
}

// dockerUserBlocksMetadata is the testable version that uses d.runCmd.
func (d *DockerBackend) dockerUserBlocksMetadata() bool {
	return dockerUserBlocksMetadataWithRunner(d.runCmd)
}

func dockerUserBlocksMetadataWithRunner(run cmdRunner) bool {
	ctx := context.Background()
	for _, args := range [][]string{
		{"iptables", "-S", "DOCKER-USER"},
		{"sudo", "iptables", "-S", "DOCKER-USER"},
	} {
		out, err := run(ctx, args[0], args[1:]...)
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
	netResult, err := d.client.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect network for metadata rule: %w", err)
	}
	if len(netResult.Network.IPAM.Config) == 0 {
		return fmt.Errorf("network %s has no IPAM config", networkName)
	}
	subnet := netResult.Network.IPAM.Config[0].Subnet

	comment := "blockyard-" + workerID
	if _, err := d.runCmd(ctx, "iptables",
		"-I", "DOCKER-USER",
		"-s", subnet.String(),
		"-d", "169.254.169.254/32",
		"-j", "DROP",
		"-m", "comment", "--comment", comment,
	); err != nil {
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
	d.deleteIptablesRulesByComment(comment)
}

// deleteIptablesRulesByComment uses `iptables -S DOCKER-USER` to find rules
// containing the given comment string, then deletes them with `iptables -D`.
// The comment must match as a complete --comment argument (quoted in the -S
// output), not as an arbitrary substring.
func (d *DockerBackend) deleteIptablesRulesByComment(comment string) {
	ctx := context.Background()
	deleteIptablesRulesByCommentWithRunner(ctx, d.runCmd, comment)
}

func deleteIptablesRulesByCommentWithRunner(ctx context.Context, run cmdRunner, comment string) {
	out, err := run(ctx, "iptables", "-S", "DOCKER-USER")
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
		if _, err := run(ctx, "iptables", args...); err != nil {
			slog.Warn("failed to delete iptables rule", "comment", comment, "error", err)
		}
	}
}

// CleanupOrphanMetadataRules removes all blockyard iptables rules left over
// from previous runs. Called at server startup. Uses prefix matching
// (without closing quote) so that all per-worker rules are matched.
func CleanupOrphanMetadataRules() {
	cleanupOrphanMetadataRulesWithRunner(context.Background(), defaultCmdRunner)
}

func cleanupOrphanMetadataRulesWithRunner(ctx context.Context, run cmdRunner) {
	out, err := run(ctx, "iptables", "-S", "DOCKER-USER")
	if err != nil {
		return
	}
	needle := `--comment "blockyard-`
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		rule := strings.TrimPrefix(line, "-A DOCKER-USER ")
		if rule == line {
			continue
		}
		args := append([]string{"-D", "DOCKER-USER"}, strings.Fields(rule)...)
		if _, err := run(ctx, "iptables", args...); err != nil {
			slog.Warn("failed to delete orphan iptables rule", "error", err)
		}
	}
}

func int64Ptr(v int64) *int64 { return &v }
