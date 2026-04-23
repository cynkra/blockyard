package server

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// Cache-failure behavior for LayeredWorkerMap (see #262). MemoryWorkerMap
// stands in for the Postgres primary; a real RedisWorkerMap is wrapped
// around a miniredis that we poison via SetError. Every method must
// still surface the primary answer — that's the whole point of making
// Postgres source-of-truth.
//
// The per-method degradation of RedisWorkerMap itself is covered in
// workermap_redis_error_test.go; this file pins the *Layered* contract:
// errors on the cache side don't corrupt the primary view.

func newLayeredWorkerMapWithErroringCache(t *testing.T) (*LayeredWorkerMap, *MemoryWorkerMap, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	primary := NewMemoryWorkerMap()
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
	primary.Set("w1", ActiveWorker{AppID: "app1"})

	mr.SetError("READONLY simulated failure")

	m.Delete("w1")
	if _, ok := primary.Get("w1"); ok {
		t.Error("primary must be cleared even if cache delete errored")
	}
}

func TestLayeredWorkerMap_CacheErrors_AggregatesReflectPrimary(t *testing.T) {
	m, primary, mr := newLayeredWorkerMapWithErroringCache(t)
	primary.Set("w1", ActiveWorker{AppID: "app1"})
	primary.Set("w2", ActiveWorker{AppID: "app1"})
	primary.Set("w3", ActiveWorker{AppID: "app2"})

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
	primary.Set("w1", ActiveWorker{AppID: "app1"})

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
// primary without user-visible impact.
func TestLayeredWorkerMap_CacheRestartRewarms(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	primary := NewMemoryWorkerMap()
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
