//go:build !minimal || docker_backend

package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cynkra/blockyard/internal/task"
)

// dockerClient is the subset of the Docker API needed by the Docker
// orchestrator variant. The concrete *client.Client satisfies this
// interface. Kept as an interface so tests can supply a mock.
type dockerClient interface {
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult
	ImagePull(ctx context.Context, refStr string, options client.ImagePullOptions) (client.ImagePullResponse, error)
}

// dockerServerFactory implements ServerFactory via Docker container
// clone. It holds the Docker client and the running server's own
// container ID so it can inspect config, pull images, and create
// sibling containers on the same networks.
type dockerServerFactory struct {
	docker   dockerClient
	serverID string // own container ID from DockerBackend.ServerID()
	version  string // server version, used as a fallback when inspect fails
	log      *slog.Logger
	// listenPort resolves the port the new container listens on;
	// set by NewDockerFactory from cfg.Server.Bind so tests can
	// wire a fake factory without the full config.
	listenPortFn func() string
}

// NewDockerFactory constructs the Docker variant of ServerFactory.
// listenPort is a closure over cfg.Server.Bind passed in from the
// wiring site (cmd/blockyard/orchestrator_docker.go).
func NewDockerFactory(c *client.Client, serverID, version string, listenPort func() string) ServerFactory {
	return &dockerServerFactory{
		docker:       c,
		serverID:     serverID,
		version:      version,
		log:          slog.Default(),
		listenPortFn: listenPort,
	}
}

// newDockerFactoryForTest constructs a Docker factory from a mock
// client for unit tests. Used by clone_docker_test.go.
func newDockerFactoryForTest(c dockerClient, serverID string, listenPort func() string) *dockerServerFactory {
	return &dockerServerFactory{
		docker:       c,
		serverID:     serverID,
		version:      "1.0.0",
		log:          slog.Default(),
		listenPortFn: listenPort,
	}
}

// dockerInstance is the newServerInstance handle for a cloned Docker
// container. Addr is resolved synchronously in CreateInstance via an
// inspect-retry loop, so the field is safe to read without further
// lookups.
type dockerInstance struct {
	id       string
	addr     string
	docker   dockerClient
	log      *slog.Logger
}

func (d *dockerInstance) ID() string   { return d.id }
func (d *dockerInstance) Addr() string { return d.addr }

func (d *dockerInstance) Kill(ctx context.Context) {
	timeout := 10
	if _, err := d.docker.ContainerStop(ctx, d.id,
		client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		d.log.Warn("stop container", "id", shortID(d.id), "error", err)
	}
	if _, err := d.docker.ContainerRemove(ctx, d.id,
		client.ContainerRemoveOptions{Force: true}); err != nil {
		d.log.Warn("remove container", "id", shortID(d.id), "error", err)
	}
}

// PreUpdate pulls the new image before the clone is created.
func (f *dockerServerFactory) PreUpdate(ctx context.Context, version string, sender task.Sender) error {
	newRef := imageWithTag(f.CurrentImageBase(ctx), version)
	sender.Write("Pulling " + newRef + " ...")
	return f.pullImage(ctx, newRef)
}

// CreateInstance clones the running container with the new image and
// extra env vars, starts it, then polls ContainerInspect until the
// container's IP appears in NetworkSettings.Networks. Returns once
// the address is cached on the dockerInstance.
func (f *dockerServerFactory) CreateInstance(
	ctx context.Context,
	ref string,
	extraEnv []string,
	sender task.Sender,
) (newServerInstance, error) {
	opts, err := f.cloneConfig(ctx, ref, extraEnv)
	if err != nil {
		return nil, err
	}

	createResult, err := f.docker.ContainerCreate(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	if _, err := f.docker.ContainerStart(ctx, createResult.ID,
		client.ContainerStartOptions{}); err != nil {
		f.killAndRemove(ctx, createResult.ID)
		return nil, fmt.Errorf("start container: %w", err)
	}

	// Inspect-retry loop until the container reports a valid IP.
	// Bounded by the ctx deadline set by the orchestrator from
	// cfg.Proxy.WorkerStartTimeout.
	addr, err := f.waitForAddr(ctx, createResult.ID)
	if err != nil {
		f.killAndRemove(ctx, createResult.ID)
		return nil, fmt.Errorf("resolve container address: %w", err)
	}

	return &dockerInstance{
		id:     createResult.ID,
		addr:   addr,
		docker: f.docker,
		log:    f.log,
	}, nil
}

// waitForAddr polls ContainerInspect until the container has a valid
// IP on one of its networks. Returns the first <ip>:<port> the
// container exposes. Times out with ctx.
func (f *dockerServerFactory) waitForAddr(ctx context.Context, id string) (string, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		if addr, err := f.containerAddr(ctx, id); err == nil {
			return addr, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// CurrentImageBase reads the image repo (without tag) from the running
// container's config. Falls back to "blockyard" on inspect errors.
func (f *dockerServerFactory) CurrentImageBase(ctx context.Context) string {
	result, err := f.docker.ContainerInspect(ctx, f.serverID, client.ContainerInspectOptions{})
	if err != nil {
		f.log.Warn("inspect self for image base", "error", err)
		return "blockyard"
	}
	img := result.Container.Config.Image
	if idx := strings.LastIndex(img, ":"); idx != -1 {
		return img[:idx]
	}
	return img
}

// CurrentImageTag reads the image tag from the running container's
// inspect result. Falls back to the factory's version on inspect
// errors (matches the pre-phase-3-8 behavior where the orchestrator
// substituted its own version string when inspect failed).
func (f *dockerServerFactory) CurrentImageTag(ctx context.Context) string {
	result, err := f.docker.ContainerInspect(ctx, f.serverID, client.ContainerInspectOptions{})
	if err != nil {
		f.log.Warn("inspect self for image tag", "error", err)
		return f.version
	}
	img := result.Container.Config.Image
	if idx := strings.LastIndex(img, ":"); idx != -1 {
		return img[idx+1:]
	}
	return f.version
}

// SupportsRollback returns true — the Docker variant can pull the
// previous version's image from the registry.
func (f *dockerServerFactory) SupportsRollback() bool { return true }

// cloneConfig inspects the current container and returns a
// ContainerCreateOptions for a new container with the given image
// and additional environment variables. This is the pre-phase-3-8
// logic, relocated from clone.go.
func (f *dockerServerFactory) cloneConfig(
	ctx context.Context,
	newImage string,
	extraEnv []string,
) (client.ContainerCreateOptions, error) {
	result, err := f.docker.ContainerInspect(ctx, f.serverID,
		client.ContainerInspectOptions{})
	if err != nil {
		return client.ContainerCreateOptions{},
			fmt.Errorf("inspect self: %w", err)
	}

	cfg := result.Container.Config
	hostCfg := result.Container.HostConfig

	// Override image.
	cfg.Image = newImage

	// Inject passive mode + mark as the new instance.
	cfg.Env = appendOrReplace(cfg.Env, "BLOCKYARD_PASSIVE", "1")
	for _, e := range extraEnv {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			cfg.Env = appendOrReplace(cfg.Env, parts[0], parts[1])
		}
	}

	// Strip host port bindings — the proxy discovers the new
	// container by Docker network/labels, so host ports are
	// unnecessary and would conflict with the still-running old
	// container.
	hostCfg.PortBindings = nil

	// Generate a unique container name to avoid conflicts.
	cfg.Hostname = ""
	name := fmt.Sprintf("blockyard-update-%d", time.Now().Unix())

	// Map network settings from inspect to create config.
	var netCfg *network.NetworkingConfig
	if result.Container.NetworkSettings != nil && len(result.Container.NetworkSettings.Networks) > 0 {
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: make(map[string]*network.EndpointSettings),
		}
		for netName, ep := range result.Container.NetworkSettings.Networks {
			netCfg.EndpointsConfig[netName] = &network.EndpointSettings{
				Aliases: ep.Aliases,
			}
		}
	}

	return client.ContainerCreateOptions{
		Name:             name,
		Config:           cfg,
		HostConfig:       hostCfg,
		NetworkingConfig: netCfg,
	}, nil
}

// pullImage pulls the given image via the Docker API.
func (f *dockerServerFactory) pullImage(ctx context.Context, ref string) error {
	resp, err := f.docker.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp)
	resp.Close()
	return nil
}

// containerAddr resolves the new container's IP address and port from
// its network settings. Returns an error when no IP is available yet
// (the container just started and Docker hasn't populated the field).
func (f *dockerServerFactory) containerAddr(ctx context.Context, containerID string) (string, error) {
	result, err := f.docker.ContainerInspect(ctx, containerID,
		client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}

	port := f.listenPortFn()

	if result.Container.NetworkSettings != nil {
		for _, ep := range result.Container.NetworkSettings.Networks {
			if ep.IPAddress.IsValid() {
				return ep.IPAddress.String() + ":" + port, nil
			}
		}
	}

	return "", fmt.Errorf("no IP address found for container %s", shortID(containerID))
}

// killAndRemove stops and removes a container. Best-effort — logs
// errors but does not return them.
func (f *dockerServerFactory) killAndRemove(ctx context.Context, containerID string) {
	timeout := 10
	if _, err := f.docker.ContainerStop(ctx, containerID,
		client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		f.log.Warn("stop container", "id", shortID(containerID), "error", err)
	}
	if _, err := f.docker.ContainerRemove(ctx, containerID,
		client.ContainerRemoveOptions{Force: true}); err != nil {
		f.log.Warn("remove container", "id", shortID(containerID), "error", err)
	}
}

// appendOrReplace sets key=value in a slice of "KEY=VALUE" strings.
// If the key already exists, its value is replaced; otherwise the
// entry is appended.
func appendOrReplace(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// shortID returns the first 12 characters of a container ID for
// concise logging, or the full string if it's already short.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
