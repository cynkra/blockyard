package preflight

import (
	"context"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
)

func TestCheckRedisOnServiceNetwork_SkipNoRedis(t *testing.T) {
	deps := DockerDeps{
		Config: &config.DockerConfig{ServiceNetwork: "my-net"},
		// RedisURL is empty — should skip.
	}
	if res := checkRedisOnServiceNetwork(context.Background(), deps); res != nil {
		t.Errorf("expected nil when RedisURL is empty, got %q", res.Message)
	}
}

func TestCheckRedisOnServiceNetwork_SkipNoServiceNetwork(t *testing.T) {
	deps := DockerDeps{
		Config:   &config.DockerConfig{}, // ServiceNetwork empty
		RedisURL: "redis://redis:6379",
	}
	if res := checkRedisOnServiceNetwork(context.Background(), deps); res != nil {
		t.Errorf("expected nil when ServiceNetwork is empty, got %q", res.Message)
	}
}

func TestCheckRedisOnServiceNetwork_SkipBadURL(t *testing.T) {
	deps := DockerDeps{
		Config:   &config.DockerConfig{ServiceNetwork: "my-net"},
		RedisURL: "://bad",
	}
	if res := checkRedisOnServiceNetwork(context.Background(), deps); res != nil {
		t.Errorf("expected nil for malformed URL, got %q", res.Message)
	}
}
