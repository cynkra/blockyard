package server

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// Cache-failure behavior for LayeredWorkerMap (see #262). Pairs a real
// PostgresWorkerMap primary with a real RedisWorkerMap cache whose
// miniredis is poisoned via SetError. Using the production primary
// (not MemoryWorkerMap) ensures the SQL path behaves correctly when
// the cache fails mid-operation.
//
// The per-method degradation of RedisWorkerMap itself is covered in
// workermap_redis_error_test.go; this file pins the *Layered* contract:
// errors on the cache side don't corrupt the primary view.
//
// Skips when BLOCKYARD_TEST_POSTGRES_URL is not set. CI's `unit` job
// always provides one.

func newLayeredWorkerMapWithErroringCache(t *testing.T) (*LayeredWorkerMap, *PostgresWorkerMap, *miniredis.Miniredis) {
	t.Helper()
	primary := NewPostgresWorkerMap(testPGDB(t), "test-host")
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	cache := NewRedisWorkerMap(client, "test-host")
	return NewLayeredWorkerMap(primary, cache), primary, mr
}

func TestLayeredWorkerMap_CacheErrors_GetFallsBackToPrimary(t *testing.T) {
	m, primary, mr := newLayeredWorkerMapWithErroringCache(t)
	primary.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})

	mr.SetError("READONLY simulated failure")

	w, ok := m.Get("w1")
	if !ok || w.AppID != "app1" {
		t.Fatalf("Get = (%+v, %v), want app1/true", w, ok)
	}
}

func TestLayeredWorkerMap_CacheErrors_SetPersistsPrimary(t *testing.T) {
	m, primary, mr := newLayeredWorkerMapWithErroringCache(t)
	mr.SetError("READONLY simulated failure")

	m.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})

	if _, ok := primary.Get("w1"); !ok {
		t.Error("primary must hold the entry even if cache write errored")
	}
}

func TestLayeredWorkerMap_CacheErrors_DeleteClearsPrimary(t *testing.T) {
	m, primary, mr := newLayeredWorkerMapWithErroringCache(t)
	primary.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})

	mr.SetError("READONLY simulated failure")

	m.Delete("w1")
	if _, ok := primary.Get("w1"); ok {
		t.Error("primary must be cleared even if cache delete errored")
	}
}

func TestLayeredWorkerMap_CacheErrors_AggregatesReflectPrimary(t *testing.T) {
	m, primary, mr := newLayeredWorkerMapWithErroringCache(t)
	primary.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})
	primary.Set("w2", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})
	primary.Set("w3", ActiveWorker{AppID: "app2", BundleID: "b2", StartedAt: time.Now()})

	mr.SetError("READONLY simulated failure")

	if n := m.Count(); n != 3 {
		t.Errorf("Count = %d, want 3", n)
	}
	if n := m.CountForApp("app1"); n != 2 {
		t.Errorf("CountForApp(app1) = %d, want 2", n)
	}
	if ids := m.ForApp("app1"); len(ids) != 2 {
		t.Errorf("ForApp(app1) = %v, want two entries", ids)
	}
}

func TestLayeredWorkerMap_CacheErrors_MarkDrainingUpdatesPrimary(t *testing.T) {
	m, primary, mr := newLayeredWorkerMapWithErroringCache(t)
	primary.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})

	mr.SetError("READONLY simulated failure")

	ids := m.MarkDraining("app1")
	if len(ids) != 1 || ids[0] != "w1" {
		t.Fatalf("MarkDraining = %v, want [w1]", ids)
	}
	w, _ := primary.Get("w1")
	if !w.Draining {
		t.Error("primary entry should be marked draining")
	}
}

// TestLayeredWorkerMap_CacheRestartRewarms simulates the DoD: Redis
// comes back empty and subsequent reads backfill the cache from the
// Postgres primary without user-visible impact.
func TestLayeredWorkerMap_CacheRestartRewarms(t *testing.T) {
	primary := NewPostgresWorkerMap(testPGDB(t), "test-host")
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	cache := NewRedisWorkerMap(client, "test-host")
	m := NewLayeredWorkerMap(primary, cache)

	m.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})

	// "Restart" Redis by wiping the dataset.
	mr.FlushAll()
	if _, ok := cache.Get("w1"); ok {
		t.Fatal("precondition: cache should be empty post-restart")
	}

	w, ok := m.Get("w1")
	if !ok || w.AppID != "app1" {
		t.Fatalf("Get after restart = (%+v, %v), want app1/true", w, ok)
	}
	if _, ok := cache.Get("w1"); !ok {
		t.Error("cache should be repopulated after the miss-through-read")
	}
}
