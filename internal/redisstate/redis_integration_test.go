//go:build redis_test

package redisstate

import (
	"context"
	"os"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
)

func testAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR not set")
	}
	return addr
}

func TestRealRedisPing(t *testing.T) {
	addr := testAddr(t)
	cfg := &config.RedisConfig{URL: "redis://" + addr + "/0", KeyPrefix: "test:"}
	c, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
