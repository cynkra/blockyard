//go:build docker_test

package docker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/preflight"
)

// TestCheckRedisOnServiceNetwork_Detected creates a Docker network with
// a container named "redis" and verifies the preflight check catches it.
func TestCheckRedisOnServiceNetwork_Detected(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	netName := fmt.Sprintf("blockyard-test-svc-%d", time.Now().UnixNano())

	// Create a test network.
	netResp, err := cli.NetworkCreate(ctx, netName, client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: true,
	})
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	defer cli.NetworkRemove(ctx, netResp.ID, client.NetworkRemoveOptions{}) //nolint:errcheck

	// Create and start a container on that network so it appears in inspect.
	ensureAlpine(t, ctx, cli)
	containerName := "blockyard-test-redis-" + fmt.Sprintf("%d", time.Now().UnixNano())
	resp, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: "alpine:latest",
			Cmd:   []string{"sleep", "30"},
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(netName),
		},
		Name: containerName,
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer cli.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true}) //nolint:errcheck

	if _, err := cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start container: %v", err)
	}

	// The check should detect the container by name.
	full := &config.Config{Docker: config.DockerConfig{ServiceNetwork: netName}}
	d := &DockerBackend{
		client:  cli,
		config:  &full.Docker,
		fullCfg: full,
		runCmd:  defaultCmdRunner,
		workers: make(map[string]*workerState),
	}
	deps := PreflightDeps{RedisURL: fmt.Sprintf("redis://%s:6379", containerName)}
	res := checkRedisOnServiceNetwork(ctx, d, deps)
	if res.Severity != preflight.SeverityError {
		t.Errorf("severity = %v, want SeverityError: %s", res.Severity, res.Message)
	}
}

// TestCheckRedisOnServiceNetwork_NotDetected verifies the check passes
// when the Redis host doesn't match any container on the network.
func TestCheckRedisOnServiceNetwork_NotDetected(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	netName := fmt.Sprintf("blockyard-test-svc-%d", time.Now().UnixNano())

	netResp, err := cli.NetworkCreate(ctx, netName, client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: true,
	})
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	defer cli.NetworkRemove(ctx, netResp.ID, client.NetworkRemoveOptions{}) //nolint:errcheck

	full := &config.Config{Docker: config.DockerConfig{ServiceNetwork: netName}}
	d := &DockerBackend{
		client:  cli,
		config:  &full.Docker,
		fullCfg: full,
		runCmd:  defaultCmdRunner,
		workers: make(map[string]*workerState),
	}
	deps := PreflightDeps{RedisURL: "redis://some-other-host:6379"}
	res := checkRedisOnServiceNetwork(ctx, d, deps)
	if res.Severity != preflight.SeverityOK {
		t.Errorf("severity = %v, want OK when Redis not on service network: %s", res.Severity, res.Message)
	}
}
