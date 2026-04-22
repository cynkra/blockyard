package server

import (
	"testing"
	"time"
)

// TestLayeredWorkerMapCacheMissPopulates verifies the read-through
// semantic: a cache miss reads from primary and warms the cache.
func TestLayeredWorkerMapCacheMissPopulates(t *testing.T) {
	primary := NewMemoryWorkerMap()
	cache := NewMemoryWorkerMap()
	m := NewLayeredWorkerMap(primary, cache)

	// Seed only the primary. Cache has nothing — simulates post-restart.
	primary.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})

	if _, ok := cache.Get("w1"); ok {
		t.Fatal("precondition: cache should be empty")
	}

	w, ok := m.Get("w1")
	if !ok || w.AppID != "app1" {
		t.Fatalf("expected primary-backed hit, got ok=%v worker=%+v", ok, w)
	}

	// Cache should now contain the entry (backfill).
	if _, ok := cache.Get("w1"); !ok {
		t.Error("cache should be populated after miss-through-read")
	}
}

// TestLayeredWorkerMapCacheHitShortCircuits verifies reads don't touch
// the primary when the cache has the entry.
func TestLayeredWorkerMapCacheHitShortCircuits(t *testing.T) {
	primary := NewMemoryWorkerMap()
	cache := NewMemoryWorkerMap()
	m := NewLayeredWorkerMap(primary, cache)

	// Cache holds app1, primary holds app2. Get must return cache value.
	cache.Set("w1", ActiveWorker{AppID: "app1"})
	primary.Set("w1", ActiveWorker{AppID: "app2"})

	w, ok := m.Get("w1")
	if !ok {
		t.Fatal("expected hit")
	}
	if w.AppID != "app1" {
		t.Errorf("AppID = %q, want %q (cache must win)", w.AppID, "app1")
	}
}

// TestLayeredWorkerMapWriteThrough verifies Set writes to both layers.
func TestLayeredWorkerMapWriteThrough(t *testing.T) {
	primary := NewMemoryWorkerMap()
	cache := NewMemoryWorkerMap()
	m := NewLayeredWorkerMap(primary, cache)

	m.Set("w1", ActiveWorker{AppID: "app1"})

	if _, ok := primary.Get("w1"); !ok {
		t.Error("primary should have the entry")
	}
	if _, ok := cache.Get("w1"); !ok {
		t.Error("cache should have the entry")
	}
}

// TestLayeredWorkerMapDeletePropagates verifies Delete clears both
// layers — otherwise a cache hit could resurrect a deleted entry.
func TestLayeredWorkerMapDeletePropagates(t *testing.T) {
	primary := NewMemoryWorkerMap()
	cache := NewMemoryWorkerMap()
	m := NewLayeredWorkerMap(primary, cache)

	m.Set("w1", ActiveWorker{AppID: "app1"})
	m.Delete("w1")

	if _, ok := primary.Get("w1"); ok {
		t.Error("primary should be empty after Delete")
	}
	if _, ok := cache.Get("w1"); ok {
		t.Error("cache should be empty after Delete")
	}
}

// TestLayeredWorkerMapAggregatesFromPrimary verifies aggregate queries
// bypass the cache and go straight to the primary. The cache may hold
// a subset and would under-report.
func TestLayeredWorkerMapAggregatesFromPrimary(t *testing.T) {
	primary := NewMemoryWorkerMap()
	cache := NewMemoryWorkerMap()
	m := NewLayeredWorkerMap(primary, cache)

	// Primary has 3, cache has 1 (partial warmup).
	primary.Set("a", ActiveWorker{AppID: "app1"})
	primary.Set("b", ActiveWorker{AppID: "app1"})
	primary.Set("c", ActiveWorker{AppID: "app2"})
	cache.Set("a", ActiveWorker{AppID: "app1"})

	if n := m.Count(); n != 3 {
		t.Errorf("Count = %d, want 3 (primary is source of truth)", n)
	}
	if n := m.CountForApp("app1"); n != 2 {
		t.Errorf("CountForApp(app1) = %d, want 2", n)
	}
}

// TestLayeredWorkerMapMarkDrainingMirrorsCache verifies MarkDraining
// on the primary also flips the cache entry so a subsequent cache hit
// sees the new draining flag. Otherwise a draining worker could still
// be routed to by a handler that reads through the cache.
func TestLayeredWorkerMapMarkDrainingMirrorsCache(t *testing.T) {
	primary := NewMemoryWorkerMap()
	cache := NewMemoryWorkerMap()
	m := NewLayeredWorkerMap(primary, cache)

	// Seed both layers with a non-draining worker.
	m.Set("w1", ActiveWorker{AppID: "app1"})

	ids := m.MarkDraining("app1")
	if len(ids) != 1 || ids[0] != "w1" {
		t.Fatalf("MarkDraining returned %v, want [w1]", ids)
	}

	// Cache must reflect the new draining state.
	w, ok := cache.Get("w1")
	if !ok || !w.Draining {
		t.Errorf("cache entry Draining = %v, want true", w.Draining)
	}
}

// TestLayeredWorkerMapClearIdleSinceReportsPrimary verifies the return
// value of ClearIdleSince reflects the primary's state — the
// authoritative "was this worker actually idle" answer.
func TestLayeredWorkerMapClearIdleSinceReportsPrimary(t *testing.T) {
	primary := NewMemoryWorkerMap()
	cache := NewMemoryWorkerMap()
	m := NewLayeredWorkerMap(primary, cache)

	primary.Set("w1", ActiveWorker{AppID: "app1"})
	primary.SetIdleSince("w1", time.Now().Add(-time.Minute))
	cache.Set("w1", ActiveWorker{AppID: "app1"}) // cache has no idle state

	if !m.ClearIdleSince("w1") {
		t.Error("ClearIdleSince should report true (primary was idle)")
	}
}
