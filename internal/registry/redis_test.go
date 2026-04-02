package registry

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

func TestRedisRegistryGetSetDelete(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	reg := NewRedisRegistry(client, 45*time.Second)

	reg.Set("worker-1", "127.0.0.1:3838")

	addr, ok := reg.Get("worker-1")
	if !ok {
		t.Fatal("expected worker to exist")
	}
	if addr != "127.0.0.1:3838" {
		t.Errorf("expected 127.0.0.1:3838, got %q", addr)
	}

	reg.Delete("worker-1")
	_, ok = reg.Get("worker-1")
	if ok {
		t.Error("expected worker to be deleted")
	}
}

func TestRedisRegistryGetMissing(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	reg := NewRedisRegistry(client, 45*time.Second)

	_, ok := reg.Get("nonexistent")
	if ok {
		t.Error("expected false for missing worker")
	}
}

func TestRedisRegistryTTLExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	reg := NewRedisRegistry(client, 10*time.Second)

	reg.Set("worker-1", "127.0.0.1:3838")

	mr.FastForward(11 * time.Second)

	_, ok := reg.Get("worker-1")
	if ok {
		t.Error("expected registry entry to expire")
	}
}

func TestRedisRegistrySetOverwrite(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	reg := NewRedisRegistry(client, 45*time.Second)

	reg.Set("worker-1", "127.0.0.1:3838")
	reg.Set("worker-1", "10.0.0.1:3838")

	addr, ok := reg.Get("worker-1")
	if !ok {
		t.Fatal("expected worker to exist")
	}
	if addr != "10.0.0.1:3838" {
		t.Errorf("expected 10.0.0.1:3838 after overwrite, got %q", addr)
	}
}

func TestRedisRegistryDeleteNonexistent(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	reg := NewRedisRegistry(client, 45*time.Second)

	// Should not panic or error.
	reg.Delete("nonexistent")
}

func TestRedisRegistryTTLRefresh(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	reg := NewRedisRegistry(client, 10*time.Second)

	reg.Set("worker-1", "127.0.0.1:3838")

	mr.FastForward(6 * time.Second)

	// Re-set refreshes TTL (simulates health poller refresh).
	reg.Set("worker-1", "127.0.0.1:3838")

	mr.FastForward(6 * time.Second)

	addr, ok := reg.Get("worker-1")
	if !ok {
		t.Error("expected worker to still exist after TTL refresh")
	}
	if addr != "127.0.0.1:3838" {
		t.Errorf("expected 127.0.0.1:3838, got %q", addr)
	}
}
