package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// cloneConfig inspects the current container and returns a
// ContainerCreateOptions for a new container with the given image
// and additional environment variables.
func (o *Orchestrator) cloneConfig(
	ctx context.Context,
	newImage string,
	extraEnv []string,
) (client.ContainerCreateOptions, error) {
	result, err := o.docker.ContainerInspect(ctx, o.serverID,
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

	// Strip host port bindings — the proxy discovers the new container
	// by Docker network/labels, so host ports are unnecessary and would
	// conflict with the still-running old container.
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

// startClone inspects self, clones config with new image +
// BLOCKYARD_PASSIVE=1, creates and starts the container.
// Returns the new container ID.
func (o *Orchestrator) startClone(ctx context.Context, image string) (string, error) {
	// Generate activation token for secure inter-server communication.
	o.activationToken = generateActivationToken()
	opts, err := o.cloneConfig(ctx, image, []string{
		"BLOCKYARD_ACTIVATION_TOKEN=" + o.activationToken,
	})
	if err != nil {
		return "", err
	}

	createResult, err := o.docker.ContainerCreate(ctx, opts)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	if _, err := o.docker.ContainerStart(ctx, createResult.ID,
		client.ContainerStartOptions{}); err != nil {
		o.killAndRemove(ctx, createResult.ID)
		return "", fmt.Errorf("start container: %w", err)
	}

	return createResult.ID, nil
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
