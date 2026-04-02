//go:build redis_test

package registry

import (
	"os"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/redisstate"
)

func testAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR not set")
	}
	return addr
}

func TestRealRedisRegistryRoundTrip(t *testing.T) {
	client := redisstate.TestClient(t, testAddr(t))
	reg := NewRedisRegistry(client, 45*time.Second)

	reg.Set("worker-1", "10.0.0.1:3838")

	addr, ok := reg.Get("worker-1")
	if !ok {
		t.Fatal("expected worker to exist")
	}
	if addr != "10.0.0.1:3838" {
		t.Errorf("expected 10.0.0.1:3838, got %q", addr)
	}

	reg.Delete("worker-1")
	if _, ok := reg.Get("worker-1"); ok {
		t.Error("expected worker to be deleted")
	}
}
