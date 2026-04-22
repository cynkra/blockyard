package session

import (
	"testing"
	"time"
)

// TestLayeredStoreCacheMissPopulates verifies the read-through
// semantic: a cache miss reads from primary and warms the cache.
func TestLayeredStoreCacheMissPopulates(t *testing.T) {
	primary := NewMemoryStore()
	cache := NewMemoryStore()
	s := NewLayeredStore(primary, cache)

	// Seed only the primary. Cache has nothing — simulates post-restart.
	primary.Set("sess-1", Entry{WorkerID: "w1", UserSub: "u", LastAccess: time.Now()})

	if _, ok := cache.Get("sess-1"); ok {
		t.Fatal("precondition: cache should be empty")
	}

	e, ok := s.Get("sess-1")
	if !ok || e.WorkerID != "w1" {
		t.Fatalf("expected primary-backed hit, got ok=%v entry=%+v", ok, e)
	}

	// Cache should now contain the entry (backfill).
	if _, ok := cache.Get("sess-1"); !ok {
		t.Error("cache should be populated after miss-through-read")
	}
}

// TestLayeredStoreCacheHitShortCircuits verifies reads don't touch
// the primary when the cache has the entry — the whole point of the
// cache layer.
func TestLayeredStoreCacheHitShortCircuits(t *testing.T) {
	primary := NewMemoryStore()
	cache := NewMemoryStore()
	s := NewLayeredStore(primary, cache)

	// Cache holds w1, primary holds w2. Get must return cache value.
	cache.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})
	primary.Set("sess-1", Entry{WorkerID: "w2", LastAccess: time.Now()})

	e, ok := s.Get("sess-1")
	if !ok {
		t.Fatal("expected hit")
	}
	if e.WorkerID != "w1" {
		t.Errorf("WorkerID = %q, want %q (cache must win)", e.WorkerID, "w1")
	}
}

// TestLayeredStoreWriteThrough verifies Set writes to both layers.
func TestLayeredStoreWriteThrough(t *testing.T) {
	primary := NewMemoryStore()
	cache := NewMemoryStore()
	s := NewLayeredStore(primary, cache)

	s.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})

	if _, ok := primary.Get("sess-1"); !ok {
		t.Error("primary should have the entry")
	}
	if _, ok := cache.Get("sess-1"); !ok {
		t.Error("cache should have the entry")
	}
}

// TestLayeredStoreDeletePropagates verifies Delete clears both layers
// — otherwise a cache hit could resurrect a deleted session.
func TestLayeredStoreDeletePropagates(t *testing.T) {
	primary := NewMemoryStore()
	cache := NewMemoryStore()
	s := NewLayeredStore(primary, cache)

	s.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})
	s.Delete("sess-1")

	if _, ok := primary.Get("sess-1"); ok {
		t.Error("primary should be empty after Delete")
	}
	if _, ok := cache.Get("sess-1"); ok {
		t.Error("cache should be empty after Delete")
	}
}

// TestLayeredStoreCountFromPrimary verifies aggregates bypass the
// cache and go straight to the primary. The cache may be an incomplete
// subset — counting it would under-report.
func TestLayeredStoreCountFromPrimary(t *testing.T) {
	primary := NewMemoryStore()
	cache := NewMemoryStore()
	s := NewLayeredStore(primary, cache)

	// Primary has 3, cache has 1 (simulating partial warmup).
	primary.Set("a", Entry{WorkerID: "w1"})
	primary.Set("b", Entry{WorkerID: "w1"})
	primary.Set("c", Entry{WorkerID: "w1"})
	cache.Set("a", Entry{WorkerID: "w1"})

	if n := s.CountForWorker("w1"); n != 3 {
		t.Errorf("CountForWorker = %d, want 3 (primary is source of truth)", n)
	}
}

// TestLayeredStoreTouchMissWhenPrimaryEmpty verifies Touch returns
// false when the primary has no entry, even if the cache does — the
// primary is authoritative for existence.
func TestLayeredStoreTouchMissWhenPrimaryEmpty(t *testing.T) {
	primary := NewMemoryStore()
	cache := NewMemoryStore()
	s := NewLayeredStore(primary, cache)

	// Cache has a stale entry the primary doesn't.
	cache.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})

	if s.Touch("sess-1") {
		t.Error("Touch should return false when primary has no entry")
	}
}
