//go:build docker_test

package preflight

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/client"

	"github.com/cynkra/blockyard/internal/backend/docker"
	"github.com/cynkra/blockyard/internal/config"
)

func testDockerClient(t *testing.T) *client.Client {
	t.Helper()
	cli, err := client.NewClientWithOpts(
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	if _, err := cli.Ping(context.Background()); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	return cli
}

func TestCheckROBindVisibility(t *testing.T) {
	cli := testDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	deps := DockerDeps{
		Client:   cli,
		MountCfg: docker.MountConfig{Mode: docker.MountModeNative},
		Config:   &config.DockerConfig{Image: "alpine:latest"},
	}

	// Pull the image first.
	if err := ensureImage(ctx, cli, deps.Config.Image); err != nil {
		t.Fatalf("pull image: %v", err)
	}

	res := checkROBindVisibility(ctx, deps)
	if res != nil {
		t.Errorf("unexpected failure: %s", res.Message)
	}
}

func TestCheckHardLink(t *testing.T) {
	t.Run("same filesystem passes", func(t *testing.T) {
		storePath := filepath.Join(t.TempDir(), ".pkg-store")
		os.MkdirAll(storePath, 0o755)
		res := checkHardLink(storePath)
		if res != nil {
			t.Errorf("unexpected failure: %s", res.Message)
		}
	})
}

func TestCheckByBuilder(t *testing.T) {
	t.Run("skipped when binary empty", func(t *testing.T) {
		deps := DockerDeps{BuilderBin: ""}
		ctx := context.Background()
		res := checkByBuilder(ctx, deps)
		if res != nil {
			t.Error("expected nil when BuilderBin is empty")
		}
	})
}
