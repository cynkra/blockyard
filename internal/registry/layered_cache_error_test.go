package registry

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// Cache-failure behavior for LayeredRegistry (see #262). Pairs a real
// PostgresRegistry primary with a real RedisRegistry cache whose
// miniredis is poisoned via SetError. Using the production primary
// (not MemoryRegistry) ensures the SQL path behaves correctly when
// the cache fails mid-operation.
//
// Skips when BLOCKYARD_TEST_POSTGRES_URL is not set. CI's `unit` job
// always provides one.

func newLayeredRegistryWithErroringCache(t *testing.T) (*LayeredRegistry, *PostgresRegistry, *miniredis.Miniredis) {
	t.Helper()
	primary := NewPostgresRegistry(testPGDB(t), time.Hour)
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	cache := NewRedisRegistry(client, time.Hour)
	return NewLayeredRegistry(primary, cache), primary, mr
}

func TestLayeredRegistry_CacheErrors_GetFallsBackToPrimary(t *testing.T) {
	r, primary, mr := newLayeredRegistryWithErroringCache(t)
	primary.Set("w1", "127.0.0.1:3838")

	mr.SetError("READONLY simulated failure")

	addr, ok := r.Get("w1")
	if !ok || addr != "127.0.0.1:3838" {
		t.Fatalf("Get = (%q, %v), want (127.0.0.1:3838, true)", addr, ok)
	}
}

func TestLayeredRegistry_CacheErrors_SetPersistsPrimary(t *testing.T) {
	r, primary, mr := newLayeredRegistryWithErroringCache(t)
	mr.SetError("READONLY simulated failure")

	r.Set("w1", "127.0.0.1:3838")

	addr, ok := primary.Get("w1")
	if !ok || addr != "127.0.0.1:3838" {
		t.Errorf("primary = (%q, %v), want (127.0.0.1:3838, true) — cache errors must not drop the write",
			addr, ok)
	}
}

func TestLayeredRegistry_CacheErrors_DeleteClearsPrimary(t *testing.T) {
	r, primary, mr := newLayeredRegistryWithErroringCache(t)
	primary.Set("w1", "127.0.0.1:3838")

	mr.SetError("READONLY simulated failure")

	r.Delete("w1")
	if _, ok := primary.Get("w1"); ok {
		t.Error("primary must be cleared even if cache delete errored")
	}
}

// TestLayeredRegistry_CacheRestartRewarms simulates the DoD: Redis
// comes back empty and the next read backfills the cache from the
// Postgres primary without user-visible impact.
func TestLayeredRegistry_CacheRestartRewarms(t *testing.T) {
	primary := NewPostgresRegistry(testPGDB(t), time.Hour)
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	cache := NewRedisRegistry(client, time.Hour)
	r := NewLayeredRegistry(primary, cache)

	r.Set("w1", "127.0.0.1:3838")

	// "Restart" Redis by wiping the dataset.
	mr.FlushAll()
	if _, ok := cache.Get("w1"); ok {
		t.Fatal("precondition: cache should be empty post-restart")
	}

	addr, ok := r.Get("w1")
	if !ok || addr != "127.0.0.1:3838" {
		t.Fatalf("Get after restart = (%q, %v), want (127.0.0.1:3838, true)", addr, ok)
	}
	if _, ok := cache.Get("w1"); !ok {
		t.Error("cache should be repopulated after the miss-through-read")
	}
}
