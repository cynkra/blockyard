//go:build docker_test

package preflight

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cynkra/blockyard/internal/config"
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

	// Create a container named "redis" on that network.
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

	// The check should detect the container by name.
	deps := DockerDeps{
		Client:   cli,
		Config:   &config.DockerConfig{ServiceNetwork: netName},
		RedisURL: fmt.Sprintf("redis://%s:6379", containerName),
	}
	res := checkRedisOnServiceNetwork(ctx, deps)
	if res == nil {
		t.Fatal("expected error when Redis container is on the service network")
	}
	if res.Severity != SeverityError {
		t.Errorf("severity = %d, want SeverityError", res.Severity)
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

	deps := DockerDeps{
		Client:   cli,
		Config:   &config.DockerConfig{ServiceNetwork: netName},
		RedisURL: "redis://some-other-host:6379",
	}
	res := checkRedisOnServiceNetwork(ctx, deps)
	if res != nil {
		t.Errorf("expected nil when Redis host is not on service network, got %q", res.Message)
	}
}
