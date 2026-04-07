package docker

import (
	"context"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/preflight"
)

// newRedisCheckBackend builds a DockerBackend stub with the configured
// service network. checkRedisOnServiceNetwork's no-Docker code paths
// short-circuit before any client call, so passing a nil client is safe
// for these unit tests.
func newRedisCheckBackend(t *testing.T, serviceNetwork string) *DockerBackend {
	t.Helper()
	full := &config.Config{Docker: config.DockerConfig{ServiceNetwork: serviceNetwork}}
	return &DockerBackend{
		client:  nil,
		config:  &full.Docker,
		fullCfg: full,
		runCmd:  defaultCmdRunner,
		workers: make(map[string]*workerState),
	}
}

func TestCheckRedisOnServiceNetwork_SkipNoRedis(t *testing.T) {
	d := newRedisCheckBackend(t, "my-net")
	deps := PreflightDeps{} // RedisURL empty
	res := checkRedisOnServiceNetwork(context.Background(), d, deps)
	if res.Severity != preflight.SeverityOK {
		t.Errorf("expected SeverityOK when RedisURL is empty, got %v: %q", res.Severity, res.Message)
	}
}

func TestCheckRedisOnServiceNetwork_SkipNoServiceNetwork(t *testing.T) {
	d := newRedisCheckBackend(t, "") // ServiceNetwork empty
	deps := PreflightDeps{RedisURL: "redis://redis:6379"}
	res := checkRedisOnServiceNetwork(context.Background(), d, deps)
	if res.Severity != preflight.SeverityOK {
		t.Errorf("expected SeverityOK when ServiceNetwork is empty, got %v: %q", res.Severity, res.Message)
	}
}

func TestCheckRedisOnServiceNetwork_SkipBadURL(t *testing.T) {
	d := newRedisCheckBackend(t, "my-net")
	deps := PreflightDeps{RedisURL: "://bad"}
	res := checkRedisOnServiceNetwork(context.Background(), d, deps)
	if res.Severity != preflight.SeverityOK {
		t.Errorf("expected SeverityOK for malformed URL, got %v: %q", res.Severity, res.Message)
	}
}

func TestCheckRedisOnServiceNetwork_SkipEmptyHost(t *testing.T) {
	d := newRedisCheckBackend(t, "my-net")
	deps := PreflightDeps{RedisURL: "redis:///0"}
	res := checkRedisOnServiceNetwork(context.Background(), d, deps)
	if res.Severity != preflight.SeverityOK {
		t.Errorf("expected SeverityOK for empty hostname, got %v: %q", res.Severity, res.Message)
	}
}
