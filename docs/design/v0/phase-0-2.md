# Phase 0-2: Docker Backend

Implement the `Backend` interface for Docker using the official Docker Go
client (`github.com/docker/docker/client`). This is the only production
backend for v0 and the foundation that all later phases depend on for
integration testing with real containers.

## Deliverables

1. `DockerBackend` struct with Docker client initialization
2. Full `Backend` interface implementation (Spawn, Stop, HealthCheck, Logs, Addr, Build)
3. Per-container bridge network creation and cleanup
4. Server multi-homing — join each worker's network to enable direct container-to-container routing
5. Container hardening (cap-drop ALL, read-only rootfs, no-new-privileges, tmpfs /tmp)
6. Image pulling (on demand, before build/spawn)
7. Label management (`dev.blockyard/*`) for resource ownership tracking
8. Orphan cleanup via `ListManaged` / `RemoveResource`
9. Server container ID detection (self-awareness for network joining)
10. Metadata endpoint protection (iptables rules blocking `169.254.169.254`)
11. Docker integration tests behind `docker_test` build tag

## Step-by-step

### Step 1: DockerBackend struct and initialization

`internal/backend/docker/docker.go` — the struct holds the Docker client,
the server's own container ID (if running in a container), config, and
per-worker internal state.

```go
package docker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/docker/docker/client"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
)

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
	metadataUnchecked metadataMode = iota
	metadataBlocked                       // iptables rule inserted successfully
	metadataUnreachable                   // already unreachable (operator rule)
	metadataUnavailable                   // iptables unavailable, endpoint unreachable — ok
)

type DockerBackend struct {
	client   *client.Client
	serverID string // own container ID; empty = native mode
	config   *config.DockerConfig

	mu      sync.RWMutex
	workers map[string]*workerState // keyed by worker ID from WorkerSpec

	metaMu   sync.Mutex
	metaMode metadataMode
}
```

**Constructor:**

```go
func New(ctx context.Context, cfg *config.DockerConfig) (*DockerBackend, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+cfg.Socket),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	// Verify connectivity
	if _, err := cli.Ping(ctx); err != nil {
		return nil, fmt.Errorf("docker ping: %w", err)
	}

	serverID := detectServerID()
	if serverID != "" {
		slog.Info("running in container mode", "server_id", serverID)
	} else {
		slog.Info("running in native mode (no server container ID detected)")
	}

	return &DockerBackend{
		client:   cli,
		serverID: serverID,
		config:   cfg,
		workers:  make(map[string]*workerState),
	}, nil
}
```

The constructor takes a `context.Context` for the ping check. API version
negotiation lets the client work with any Docker daemon version without
hardcoding an API version.

**Interface compliance:** a compile-time check ensures `DockerBackend`
satisfies `backend.Backend`:

```go
var _ backend.Backend = (*DockerBackend)(nil)
```

### Step 2: Server container ID detection

The server needs its own container ID to join worker networks. Detection
order (first match wins):

1. `BLOCKYARD_SERVER_ID` env var — explicit override for non-standard setups
2. Parse `/proc/self/cgroup` — Docker writes the container ID in cgroup paths
3. Read hostname — Docker sets it to the short container ID by default
4. If all fail: empty string — native mode. Skip network joining; workers
   are reachable on the bridge gateway IP.

```go
func detectServerID() string {
	// 1. Explicit env var
	if id := os.Getenv("BLOCKYARD_SERVER_ID"); id != "" {
		slog.Info("server ID from env", "container_id", id)
		return id
	}

	// 2. Parse /proc/self/cgroup
	if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		if id := extractContainerIDFromCgroup(string(data)); id != "" {
			slog.Info("server ID from cgroup", "container_id", id)
			return id
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

func extractContainerIDFromCgroup(content string) string {
	for _, line := range strings.Split(content, "\n") {
		parts := strings.Split(line, "/")
		for i, part := range parts {
			switch {
			case part == "docker" && i+1 < len(parts):
				candidate := parts[i+1]
				if len(candidate) >= 12 && isHex(candidate) {
					return candidate
				}
			case strings.HasPrefix(part, "docker-") && strings.HasSuffix(part, ".scope"):
				// docker-<id>.scope
				candidate := strings.TrimPrefix(part, "docker-")
				candidate = strings.TrimSuffix(candidate, ".scope")
				if len(candidate) >= 12 && isHex(candidate) {
					return candidate
				}
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
```

### Step 3: Label conventions

All blockyard-managed resources (containers and networks) carry labels for
discovery and cleanup. Labels use the `dev.blockyard/` prefix:

| Label | Value | Applied to |
|---|---|---|
| `dev.blockyard/managed` | `true` | All containers and networks |
| `dev.blockyard/app-id` | `{app_id}` | Worker and build containers, networks |
| `dev.blockyard/worker-id` | `{worker_id}` | Worker containers, networks |
| `dev.blockyard/bundle-id` | `{bundle_id}` | Build containers |
| `dev.blockyard/role` | `worker` or `build` | All containers |

These labels serve two purposes:
- **Orphan cleanup:** `ListManaged()` queries for `dev.blockyard/managed=true`
- **Debugging:** `docker ps --filter label=dev.blockyard/app-id=...`

Helper functions to construct label maps:

```go
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
```

### Step 4: Image pulling

On-demand image pulling before `Spawn` and `Build`. Pulls only if the image
is not already present locally. Uses the Docker client's `ImagePull` which
returns an `io.ReadCloser` that must be fully consumed for the pull to
complete.

```go
func (d *DockerBackend) ensureImage(ctx context.Context, image string) error {
	// Check if image exists locally
	_, _, err := d.client.ImageInspectWithRaw(ctx, image)
	if err == nil {
		return nil // already present
	}

	slog.Info("pulling image", "image", image)
	reader, err := d.client.ImagePull(ctx, image, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", image, err)
	}
	defer reader.Close()

	// Must consume the reader for the pull to complete.
	// Discard output — pull progress is not surfaced to callers.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("pull image %s: %w", image, err)
	}

	slog.Info("image pulled", "image", image)
	return nil
}
```

The correct import for `PullOptions` is `github.com/docker/docker/api/types/image`.

### Step 5: Spawn — create network, create container, join, start

The spawn flow creates an isolated network per worker, creates a hardened
container attached to that network, optionally joins the server to the
network, and starts the container.

```go
func (d *DockerBackend) Spawn(ctx context.Context, spec backend.WorkerSpec) error {
	// 1. Ensure image exists locally
	if err := d.ensureImage(ctx, spec.Image); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}

	networkName := "blockyard-" + spec.WorkerID

	// 2. Create per-worker bridge network
	networkID, err := d.createNetwork(ctx, networkName, spec.AppID, spec.WorkerID)
	if err != nil {
		return fmt.Errorf("spawn: %w", err)
	}

	// 3. Block metadata endpoint (iptables)
	if err := d.blockMetadataEndpoint(ctx, networkName, spec.WorkerID); err != nil {
		// Clean up the network on failure
		_ = d.client.NetworkRemove(ctx, networkID)
		return fmt.Errorf("spawn: metadata block: %w", err)
	}

	// 4. Create container
	containerID, err := d.createWorkerContainer(ctx, spec, networkName)
	if err != nil {
		_ = d.removeMetadataRule(spec.WorkerID)
		_ = d.client.NetworkRemove(ctx, networkID)
		return fmt.Errorf("spawn: %w", err)
	}

	// 5. Join server to worker network (if running in a container)
	if d.serverID != "" {
		if err := d.joinNetwork(ctx, d.serverID, networkName); err != nil {
			_ = d.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
			_ = d.removeMetadataRule(spec.WorkerID)
			_ = d.client.NetworkRemove(ctx, networkID)
			return fmt.Errorf("spawn: %w", err)
		}
	}

	// 6. Start the container
	if err := d.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		if d.serverID != "" {
			_ = d.disconnectNetwork(ctx, d.serverID, networkName)
		}
		_ = d.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		_ = d.removeMetadataRule(spec.WorkerID)
		_ = d.client.NetworkRemove(ctx, networkID)
		return fmt.Errorf("spawn: start container: %w", err)
	}

	// Record internal state
	d.mu.Lock()
	d.workers[spec.WorkerID] = &workerState{
		containerID: containerID,
		networkID:   networkID,
		networkName: networkName,
	}
	d.mu.Unlock()

	return nil
}
```

**Rollback on failure:** each step cleans up resources created by prior steps
if it fails. This prevents leaked networks or containers when spawn is
partially successful.

**Network creation:**

```go
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
```

**Container creation (hardened):**

```go
func (d *DockerBackend) createWorkerContainer(
	ctx context.Context,
	spec backend.WorkerSpec,
	networkName string,
) (string, error) {
	containerName := "blockyard-worker-" + spec.WorkerID

	// Build bind mounts
	binds := []string{
		spec.BundlePath + ":" + spec.WorkerMount + ":ro",
	}
	if spec.LibraryPath != "" {
		binds = append(binds, spec.LibraryPath+":/blockyard-lib:ro")
	}

	// Environment
	env := []string{
		fmt.Sprintf("SHINY_PORT=%d", spec.ShinyPort),
		"R_LIBS=/blockyard-lib",
	}

	// Resource limits
	var resources container.Resources
	if spec.MemoryLimit != "" {
		if mem, ok := parseMemoryLimit(spec.MemoryLimit); ok {
			resources.Memory = mem
		}
	}
	if spec.CPULimit > 0 {
		resources.NanoCPUs = int64(spec.CPULimit * 1e9)
	}

	resp, err := d.client.ContainerCreate(ctx,
		&container.Config{
			Image:  spec.Image,
			Cmd:    spec.Cmd,
			Env:    env,
			Labels: workerLabels(spec),
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(networkName),
			Binds:       binds,
			Tmpfs:       map[string]string{"/tmp": ""},
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			Resources:      resources,
		},
		nil, // no network config beyond NetworkMode
		nil, // no platform
		containerName,
	)
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", containerName, err)
	}

	return resp.ID, nil
}
```

**Memory limit parsing:**

```go
// parseMemoryLimit converts human-readable memory strings like "512m", "1g",
// "256mb" to bytes. Returns (bytes, true) on success.
func parseMemoryLimit(s string) (int64, bool) {
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
```

**Network joining and disconnecting:**

```go
func (d *DockerBackend) joinNetwork(ctx context.Context, containerID, networkName string) error {
	return d.client.NetworkConnect(ctx, networkName, containerID, nil)
}

func (d *DockerBackend) disconnectNetwork(ctx context.Context, containerID, networkName string) error {
	return d.client.NetworkDisconnect(ctx, networkName, containerID, true)
}
```

### Step 6: Addr — resolve worker IP on its named network

The worker's IP address must be looked up on the specific `blockyard-*`
network, not just any network the container is attached to.

```go
func (d *DockerBackend) Addr(ctx context.Context, id string) (string, error) {
	d.mu.RLock()
	ws, ok := d.workers[id]
	d.mu.RUnlock()
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
```

Returns `"host:port"` as a string, matching the `Backend` interface
signature. The port comes from `DockerConfig.ShinyPort` (default 3838).

### Step 7: Stop — stop container, disconnect server, remove network

Best-effort on each step — failures in later steps don't prevent earlier
cleanup from completing.

```go
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

	// 3. Remove iptables metadata rule
	if err := d.removeMetadataRule(id); err != nil {
		slog.Warn("failed to remove metadata rule", "worker_id", id, "error", err)
	}

	// 4. Disconnect server from the worker's network
	if d.serverID != "" {
		if err := d.disconnectNetwork(ctx, d.serverID, ws.networkName); err != nil {
			slog.Warn("failed to disconnect from network", "worker_id", id, "error", err)
		}
	}

	// 5. Remove the network
	if err := d.client.NetworkRemove(ctx, ws.networkName); err != nil {
		slog.Warn("failed to remove network", "worker_id", id, "error", err)
	}

	return firstErr
}
```

**Stop ordering matters.** Stop container first, remove container,
disconnect server from network, remove network. If you remove the network
before disconnecting, the network removal fails because it still has
connected endpoints.

The worker state is removed from the internal map first (under lock) so
concurrent calls to `Addr` or `HealthCheck` for this worker return
immediately rather than racing with cleanup.

### Step 8: HealthCheck — TCP probe

A simple TCP connection attempt to the worker's Shiny port. If the
connection succeeds, the worker is healthy. No HTTP-level check — Shiny
doesn't expose a health endpoint.

```go
func (d *DockerBackend) HealthCheck(ctx context.Context, id string) bool {
	addr, err := d.Addr(ctx, id)
	if err != nil {
		return false
	}

	// 10s timeout — must not stall the health polling loop (which runs
	// every 15s by default).
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
```

### Step 9: Logs — stream container stdout/stderr

Returns a `backend.LogStream` with a channel of log lines and a close
function. A goroutine reads from the Docker log stream and sends lines on
the channel until the container exits or `Close` is called.

```go
func (d *DockerBackend) Logs(ctx context.Context, id string) (backend.LogStream, error) {
	d.mu.RLock()
	ws, ok := d.workers[id]
	d.mu.RUnlock()
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
```

**Docker log stream demuxing:** Docker's container log stream uses a
multiplexed format — each frame has an 8-byte header indicating the stream
(stdout/stderr) and the frame length. The `stdcopy` package from the Docker
SDK handles this:

```go
func demuxReader(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		// stdcopy.StdCopy demuxes the Docker log stream, writing
		// both stdout and stderr to pw.
		_, err := stdcopy.StdCopy(pw, pw, r)
		pw.CloseWithError(err)
	}()
	return pr
}
```

Import: `github.com/docker/docker/pkg/stdcopy`.

### Step 10: Build — run a build container to completion

The build flow creates a short-lived container that runs `rv sync` for
dependency restoration. The container mounts the bundle read-only and a
library output directory read-write. Build containers do not get their own
network — they use the default bridge (they need internet access to download
rv and packages but don't need to be proxied).

```go
func (d *DockerBackend) Build(ctx context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
	// 1. Ensure image exists locally
	if err := d.ensureImage(ctx, spec.Image); err != nil {
		return backend.BuildResult{}, fmt.Errorf("build: %w", err)
	}

	containerName := "blockyard-build-" + spec.BundleID

	// Download rv and run sync in one shot
	rvURL := fmt.Sprintf(
		"https://github.com/a2-ai/rv/releases/download/%s/rv-x86_64-unknown-linux-gnu",
		spec.RvVersion,
	)
	cmd := []string{
		"sh", "-c",
		fmt.Sprintf(
			"curl -sSL %s -o /usr/local/bin/rv && chmod +x /usr/local/bin/rv && rv sync",
			rvURL,
		),
	}

	// 2. Create container
	resp, err := d.client.ContainerCreate(ctx,
		&container.Config{
			Image:      spec.Image,
			Cmd:        cmd,
			WorkingDir: "/app",
			Labels:     buildLabels(spec),
		},
		&container.HostConfig{
			Binds: []string{
				spec.BundlePath + ":/app:ro",
				spec.LibraryPath + ":/app/rv/library:rw",
			},
			Tmpfs: map[string]string{
				"/tmp":           "",
				"/root/.cache/rv": "",
			},
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			// Rootfs NOT read-only — needs to install rv binary to /usr/local/bin
		},
		nil, nil,
		containerName,
	)
	if err != nil {
		return backend.BuildResult{}, fmt.Errorf("build: create container: %w", err)
	}

	containerID := resp.ID

	// 3. Start the build container
	if err := d.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		_ = d.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		return backend.BuildResult{}, fmt.Errorf("build: start container: %w", err)
	}

	// 4. Wait for exit
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

	// 5. Remove the build container
	_ = d.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})

	return backend.BuildResult{
		Success:  success,
		ExitCode: exitCode,
	}, nil
}
```

**Build and worker containers use the same image.** The configured image
(`[docker] image`) is a standard rocker image (e.g. `rocker/r-ver`) — it
ships R but not rv. The build container downloads rv from GitHub releases
as the first step of its command, then runs `rv sync`.

Using the same base image for builds and workers guarantees that the R
version, architecture, and system libraries are identical — which means
rv's namespaced library path (`rv/library/<R version>/<arch>/<codename>`)
resolves to the same directory in both containers.

### Step 11: ListManaged and RemoveResource — orphan cleanup

Queries Docker for all containers and networks labeled with
`dev.blockyard/managed=true`. Used at startup to clean up resources left
behind by a previous server crash.

```go
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
			ID:   c.ID,
			Kind: backend.ResourceContainer,
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
			ID:   n.ID,
			Kind: backend.ResourceNetwork,
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
```

Import: `github.com/docker/docker/api/types/filters`.

### Step 12: Metadata endpoint protection

Cloud providers expose instance metadata at `169.254.169.254`. Without
protection, R code in a container can steal host IAM credentials. The
backend inserts an iptables rule in the `DOCKER-USER` chain scoped to the
worker network's subnet, tagged with a comment for cleanup.

The detection result is cached after the first spawn — we check once whether
iptables is available and whether the metadata endpoint is reachable, then
reuse that result for all subsequent spawns.

```go
func (d *DockerBackend) blockMetadataEndpoint(ctx context.Context, networkName, workerID string) error {
	d.metaMu.Lock()
	defer d.metaMu.Unlock()

	switch d.metaMode {
	case metadataBlocked:
		// iptables works — insert a rule for this worker
		return d.insertMetadataRule(ctx, networkName, workerID)
	case metadataUnreachable:
		// Already unreachable by operator rule — nothing to do
		return nil
	case metadataUnavailable:
		// Already checked, can't block, but endpoint was unreachable — ok
		return nil
	}

	// First spawn — detect capabilities
	if tryIptables() {
		d.metaMode = metadataBlocked
		return d.insertMetadataRule(ctx, networkName, workerID)
	}

	// iptables unavailable — check if endpoint is already unreachable
	if !isMetadataReachable() {
		slog.Info("metadata endpoint unreachable (operator rule); no iptables needed")
		d.metaMode = metadataUnreachable
		return nil
	}

	// iptables unavailable AND endpoint is reachable — fail
	return fmt.Errorf(
		"metadata endpoint 169.254.169.254 is reachable and iptables is unavailable; " +
			"add CAP_NET_ADMIN or install a blanket iptables rule to block it",
	)
}

func tryIptables() bool {
	err := exec.Command("iptables", "-L", "DOCKER-USER", "-n").Run()
	return err == nil
}

func isMetadataReachable() bool {
	conn, err := net.DialTimeout("tcp", "169.254.169.254:80", 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (d *DockerBackend) insertMetadataRule(ctx context.Context, networkName, workerID string) error {
	// Get the network's subnet
	info, err := d.client.NetworkInspect(ctx, networkName, network.InspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect network for metadata rule: %w", err)
	}

	if len(info.IPAM.Config) == 0 {
		return fmt.Errorf("network %s has no IPAM config", networkName)
	}
	subnet := info.IPAM.Config[0].Subnet

	comment := "blockyard-" + workerID
	err = exec.CommandContext(ctx, "iptables",
		"-I", "DOCKER-USER",
		"-s", subnet,
		"-d", "169.254.169.254",
		"-j", "DROP",
		"-m", "comment", "--comment", comment,
	).Run()
	if err != nil {
		return fmt.Errorf("insert iptables rule: %w", err)
	}

	return nil
}

func (d *DockerBackend) removeMetadataRule(workerID string) error {
	if d.metaMode != metadataBlocked {
		return nil
	}

	comment := "blockyard-" + workerID

	// List rules, find the one with our comment, delete it
	out, err := exec.Command("iptables", "-L", "DOCKER-USER", "-n", "--line-numbers",
		"-m", "comment", "--comment", comment).Output()
	if err != nil {
		return nil // best-effort
	}

	// Parse line numbers and delete in reverse order
	lines := strings.Split(string(out), "\n")
	var ruleNums []int
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			if num, err := strconv.Atoi(fields[0]); err == nil {
				ruleNums = append(ruleNums, num)
			}
		}
	}

	// Delete in reverse order to preserve line numbers
	sort.Sort(sort.Reverse(sort.IntSlice(ruleNums)))
	for _, num := range ruleNums {
		_ = exec.Command("iptables", "-D", "DOCKER-USER", strconv.Itoa(num)).Run()
	}

	return nil
}
```

The iptables approach is the same as described in the plan: scoped to the
network subnet, tagged with a `blockyard-{worker-id}` comment for cleanup.
If iptables is unavailable (no `CAP_NET_ADMIN`), the backend falls back to
a reachability check. If the endpoint is already unreachable
(operator-installed blanket rule), spawn proceeds. If it's reachable and
iptables can't block it, spawn fails.

### Step 13: Docker integration tests

Tests that exercise the real Docker backend. Gated behind a `docker_test`
build tag. Run with `go test -tags docker_test ./internal/backend/docker/`.
Regular `go test ./...` skips them.

`internal/backend/docker/docker_test.go`:

```go
//go:build docker_test

package docker

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
)

func testConfig() *config.DockerConfig {
	return &config.DockerConfig{
		Socket:    "/var/run/docker.sock",
		Image:     "alpine:latest",
		ShinyPort: 8080,
		RvVersion: "latest",
	}
}

func TestSpawnAndStop(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       "alpine:latest",
		Cmd:         []string{"sleep", "300"}, // stay alive
		BundlePath:  "/tmp",
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		Labels:      map[string]string{},
	}

	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Container should have an address
	addr, err := b.Addr(ctx, workerID)
	if err != nil {
		t.Fatalf("Addr: %v", err)
	}
	if addr == "" {
		t.Fatal("Addr returned empty string")
	}
	t.Logf("worker addr: %s", addr)

	// Stop and clean up
	if err := b.Stop(ctx, workerID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Addr should fail after stop
	if _, err := b.Addr(ctx, workerID); err == nil {
		t.Fatal("Addr should fail after Stop")
	}
}

func TestHealthCheckNoListener(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       "alpine:latest",
		Cmd:         []string{"sleep", "300"}, // alive but not listening
		BundlePath:  "/tmp",
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		Labels:      map[string]string{},
	}

	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer b.Stop(ctx, workerID)

	// Give the container a moment to start
	time.Sleep(500 * time.Millisecond)

	// Health check should fail — nothing listening on port
	if b.HealthCheck(ctx, workerID) {
		t.Fatal("HealthCheck should return false when nothing is listening")
	}
}

func TestOrphanCleanup(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	workerID := "test-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       "test-app",
		WorkerID:    workerID,
		Image:       "alpine:latest",
		Cmd:         []string{"sleep", "300"},
		BundlePath:  "/tmp",
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		Labels:      map[string]string{},
	}

	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Simulate crash — don't call Stop, just list and clean up
	managed, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(managed) == 0 {
		t.Fatal("expected managed resources after spawn")
	}

	for _, r := range managed {
		if err := b.RemoveResource(ctx, r); err != nil {
			t.Logf("RemoveResource warning: %v", err)
		}
	}

	// Should be clean now
	remaining, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged after cleanup: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected 0 remaining resources, got %d", len(remaining))
	}
}

func TestMemoryLimitParsing(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"512m", 512 * 1024 * 1024},
		{"1g", 1024 * 1024 * 1024},
		{"256mb", 256 * 1024 * 1024},
		{"100kb", 100 * 1024},
		{"1024", 1024},
	}

	for _, tt := range tests {
		got, ok := parseMemoryLimit(tt.input)
		if !ok {
			t.Errorf("parseMemoryLimit(%q) returned !ok", tt.input)
			continue
		}
		if got != tt.want {
			t.Errorf("parseMemoryLimit(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestNetworkIsolation(t *testing.T) {
	ctx := context.Background()
	b, err := New(ctx, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Spawn two workers
	id1 := "test-" + uuid.New().String()[:8]
	id2 := "test-" + uuid.New().String()[:8]
	makeSpec := func(id string) backend.WorkerSpec {
		return backend.WorkerSpec{
			AppID:       "test-app",
			WorkerID:    id,
			Image:       "alpine:latest",
			Cmd:         []string{"sleep", "300"},
			BundlePath:  "/tmp",
			LibraryPath: "",
			WorkerMount: "/app",
			ShinyPort:   8080,
			Labels:      map[string]string{},
		}
	}

	if err := b.Spawn(ctx, makeSpec(id1)); err != nil {
		t.Fatalf("Spawn worker 1: %v", err)
	}
	defer b.Stop(ctx, id1)

	if err := b.Spawn(ctx, makeSpec(id2)); err != nil {
		t.Fatalf("Spawn worker 2: %v", err)
	}
	defer b.Stop(ctx, id2)

	// Get their addresses
	addr1, err := b.Addr(ctx, id1)
	if err != nil {
		t.Fatalf("Addr worker 1: %v", err)
	}
	addr2, err := b.Addr(ctx, id2)
	if err != nil {
		t.Fatalf("Addr worker 2: %v", err)
	}

	// Workers should be on different networks (different IPs from different subnets)
	if addr1 == addr2 {
		t.Fatalf("workers should have different addresses, both got %s", addr1)
	}

	// Verify they cannot reach each other by running a connectivity check
	// from worker 1's container toward worker 2's IP
	ip2 := strings.Split(addr2, ":")[0]
	execResp, err := b.client.ContainerExecCreate(ctx, b.workers[id1].containerID,
		container.ExecOptions{
			Cmd: []string{"sh", "-c", fmt.Sprintf(
				"wget -q -O /dev/null --timeout=2 http://%s:%d/ 2>&1 || exit 1",
				ip2, 8080,
			)},
		},
	)
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}

	if err := b.client.ContainerExecStart(ctx, execResp.ID, container.ExecStartOptions{}); err != nil {
		t.Fatalf("ExecStart: %v", err)
	}

	// Wait for exec to complete and check exit code
	time.Sleep(3 * time.Second)
	inspect, err := b.client.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		t.Fatalf("ExecInspect: %v", err)
	}

	// Exit code should be non-zero (connection failed = isolated)
	if inspect.ExitCode == 0 {
		t.Fatal("worker 1 should NOT be able to reach worker 2 (network isolation broken)")
	}
}
```

**Note on TestMemoryLimitParsing:** this test does not require Docker and
does not need the `docker_test` build tag, but it's placed here alongside
the code it tests. Alternatively it could go in a `_test.go` file without
the build tag — either works since the function is unexported and must be
tested from the same package.

### Step 14: CI configuration

The CI workflow already has two jobs from phase 0-1. The `docker-tests` job
should be enabled:

```yaml
name: CI
on: [push, pull_request]

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
      - run: go vet ./...
      - run: go test ./...

  docker-tests:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
      - run: docker pull alpine:latest
      - run: go test -tags docker_test ./...
```

Pre-pulling `alpine:latest` avoids timeout flakiness in tests.

## Container hardening summary

All blockyard-managed containers (workers and builds) apply these security
settings:

| Setting | Value | Why |
|---|---|---|
| `CapDrop` | `ALL` | Drop all Linux capabilities — Shiny needs none |
| `SecurityOpt` | `no-new-privileges` | Prevent privilege escalation via setuid/setgid |
| `ReadonlyRootfs` | `true` (workers only) | Prevent filesystem writes outside mounts |
| `Tmpfs` | `/tmp` (all), `/root/.cache/rv` (builds) | Writable scratch space |
| Published ports | None | Workers are only reachable via the bridge network |
| Network | Per-worker bridge | Workers cannot reach each other |

Build containers do not use `ReadonlyRootfs` — they need to download and
install the `rv` binary to `/usr/local/bin` before running `rv sync`. The
build container is short-lived and discarded after completion.

## Network topology

```
                  ┌─────────────────┐
                  │  blockyard      │
                  │  server          │
                  │  (container or  │
                  │   native)       │
                  └──┬──┬──┬───────┘
                     │  │  │
          ┌──────────┘  │  └──────────┐
          │             │             │
   ┌──────▼──────┐ ┌───▼──────┐ ┌────▼─────┐
   │ blockyard-  │ │ blockyard-│ │ blockyard-│
   │ {worker-1}  │ │ {worker-2}│ │ {worker-3}│
   │ (bridge)    │ │ (bridge)  │ │ (bridge)  │
   └──────┬──────┘ └───┬──────┘ └────┬─────┘
          │             │             │
   ┌──────▼──────┐ ┌───▼──────┐ ┌────▼─────┐
   │  worker-1   │ │ worker-2 │ │ worker-3 │
   │  container  │ │ container│ │ container│
   └─────────────┘ └──────────┘ └──────────┘
```

Each worker gets its own bridge network. The server is multi-homed — it
joins every worker network so it can route to any worker. Workers are
isolated from each other (no shared network).

When the server is running natively (not in a container), network joining is
skipped. Workers are reachable on the Docker bridge gateway IP instead.

## Imports summary

The Docker Go client uses a different package layout from v25+. Key imports
for this phase:

```go
import (
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)
```

These are already covered by the `github.com/docker/docker` dependency in
`go.mod` from phase 0-1.

## Exit criteria

Phase 0-2 is done when:

- `New()` connects to Docker and detects server ID
- `Spawn()` creates a network + hardened container, records internal state
- `Addr()` resolves the worker's IP on its named network
- `Stop()` cleans up the container, network, and iptables rule (best-effort)
- `HealthCheck()` returns `true`/`false` based on TCP reachability
- `Logs()` streams container stdout/stderr with proper demuxing
- `Build()` runs a build container to completion and returns exit code
- `ensureImage()` pulls on demand, skips if already present
- `ListManaged()` discovers all labeled containers and networks
- `RemoveResource()` removes containers and networks by ID
- Metadata endpoint protection blocks `169.254.169.254` or verifies it's
  already unreachable
- Memory limit parsing handles common units (`m`, `g`, `mb`, `gb`)
- All Docker integration tests pass with a real Docker daemon
- `docker_test` CI job is enabled and green
- `go test ./...` still passes (mock backend unaffected)
- `go vet ./...` clean

## Implementation notes

- **Docker client version:** `github.com/docker/docker v27.5.1+incompatible`
  is already in `go.mod` from phase 0-1. No version change needed.

- **Error handling:** all Docker API calls wrap errors with `fmt.Errorf`
  including context (container ID, network name, operation). The error
  messages are human-readable for `slog` output.

- **Stop ordering matters.** When stopping a worker: stop container first,
  remove container, remove iptables rule, disconnect server from network,
  remove network. If you remove the network before disconnecting the server,
  the network removal fails because it still has connected endpoints.

- **Build container cleanup.** The build container is removed after
  completion regardless of success or failure. We don't use Docker's
  `AutoRemove` because we need to read the exit code before the container
  disappears.

- **Native mode (no server ID).** When the server runs outside Docker,
  `Spawn()` and `Stop()` skip the network join/disconnect steps.
  `Addr()` returns the worker's IP on its bridge network, which must be
  routable from the host for native mode to work. This is the case on
  Linux. If container IPs are not routable from the host, run the server
  inside a container (e.g. the devcontainer) instead.

- **rv library path namespacing.** rv's default library path is
  `<project>/rv/library/<R version>/<arch>/<codename>` (e.g.
  `/app/rv/library/4.4/x86_64-pc-linux-gnu/jammy`). The build container
  mounts the host library dir at `/app/rv/library` and rv creates the
  namespaced subdirectories inside it. Two things to verify during testing:
  1. That rv correctly creates and populates the namespaced subdirectory
     under the mount point.
  2. That the worker container's R process finds the library at runtime —
     the worker container sets `R_LIBS=/blockyard-lib` so R includes the
     restored library in `.libPaths()`.

- **`workerState` map is internal.** The `workers` map is not exported.
  Callers interact only via string worker IDs through the `Backend`
  interface. The map is protected by `sync.RWMutex` — `Addr`, `Logs`, and
  `HealthCheck` take read locks; `Spawn` and `Stop` take write locks.

- **Build logs are not streamed in this phase.** The `Build` method waits
  for the container to exit but does not stream its logs. Log streaming for
  build containers is wired in phase 0-3 (content management) where the
  task store provides the streaming infrastructure. Phase 0-2's `Build`
  returns success/failure and exit code only.
