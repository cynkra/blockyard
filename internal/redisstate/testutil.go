package redisstate

import (
	"context"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
)

// TestClient creates a redisstate.Client connected to the given addr
// (typically miniredis) with no authentication. Exported so
// session/registry/server test packages can use it.
func TestClient(t *testing.T, addr string) *Client {
	t.Helper()
	cfg := &config.RedisConfig{URL: "redis://" + addr + "/0", KeyPrefix: "test:"}
	c, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal("redisstate.TestClient:", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
