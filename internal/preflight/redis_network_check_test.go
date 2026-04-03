package preflight

import (
	"context"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
)

func TestCheckRedisOnServiceNetwork_SkipNoRedis(t *testing.T) {
	deps := DockerDeps{
		Config: &config.DockerConfig{ServiceNetwork: "my-net"},
		// RedisURL is empty — should return OK.
	}
	res := checkRedisOnServiceNetwork(context.Background(), deps)
	if res.Severity != SeverityOK {
		t.Errorf("expected SeverityOK when RedisURL is empty, got %v: %q", res.Severity, res.Message)
	}
}

func TestCheckRedisOnServiceNetwork_SkipNoServiceNetwork(t *testing.T) {
	deps := DockerDeps{
		Config:   &config.DockerConfig{}, // ServiceNetwork empty
		RedisURL: "redis://redis:6379",
	}
	res := checkRedisOnServiceNetwork(context.Background(), deps)
	if res.Severity != SeverityOK {
		t.Errorf("expected SeverityOK when ServiceNetwork is empty, got %v: %q", res.Severity, res.Message)
	}
}

func TestCheckRedisOnServiceNetwork_SkipBadURL(t *testing.T) {
	deps := DockerDeps{
		Config:   &config.DockerConfig{ServiceNetwork: "my-net"},
		RedisURL: "://bad",
	}
	res := checkRedisOnServiceNetwork(context.Background(), deps)
	if res.Severity != SeverityOK {
		t.Errorf("expected SeverityOK for malformed URL, got %v: %q", res.Severity, res.Message)
	}
}

func TestCheckRedisOnServiceNetwork_SkipEmptyHost(t *testing.T) {
	deps := DockerDeps{
		Config:   &config.DockerConfig{ServiceNetwork: "my-net"},
		RedisURL: "redis:///0", // valid URL but empty hostname
	}
	res := checkRedisOnServiceNetwork(context.Background(), deps)
	if res.Severity != SeverityOK {
		t.Errorf("expected SeverityOK for empty hostname, got %v: %q", res.Severity, res.Message)
	}
}
