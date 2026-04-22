package registry

import "testing"

// TestLayeredRegistryCacheMissPopulates verifies the read-through
// semantic: a cache miss reads from primary and warms the cache.
func TestLayeredRegistryCacheMissPopulates(t *testing.T) {
	primary := NewMemoryRegistry()
	cache := NewMemoryRegistry()
	r := NewLayeredRegistry(primary, cache)

	// Seed only the primary. Cache has nothing — simulates post-restart.
	primary.Set("w1", "127.0.0.1:3838")

	if _, ok := cache.Get("w1"); ok {
		t.Fatal("precondition: cache should be empty")
	}

	addr, ok := r.Get("w1")
	if !ok || addr != "127.0.0.1:3838" {
		t.Fatalf("expected primary-backed hit, got ok=%v addr=%q", ok, addr)
	}

	// Cache should now contain the entry (backfill).
	if _, ok := cache.Get("w1"); !ok {
		t.Error("cache should be populated after miss-through-read")
	}
}

// TestLayeredRegistryCacheHitShortCircuits confirms reads don't touch
// the primary when the cache has the entry — the whole point of the
// cache layer.
func TestLayeredRegistryCacheHitShortCircuits(t *testing.T) {
	primary := NewMemoryRegistry()
	cache := NewMemoryRegistry()
	r := NewLayeredRegistry(primary, cache)

	// Cache holds one address, primary another. Get must return cache value.
	cache.Set("w1", "10.0.0.1:3838")
	primary.Set("w1", "127.0.0.1:3838")

	addr, ok := r.Get("w1")
	if !ok {
		t.Fatal("expected hit")
	}
	if addr != "10.0.0.1:3838" {
		t.Errorf("addr = %q, want %q (cache must win)", addr, "10.0.0.1:3838")
	}
}

// TestLayeredRegistryWriteThrough verifies Set writes to both layers.
func TestLayeredRegistryWriteThrough(t *testing.T) {
	primary := NewMemoryRegistry()
	cache := NewMemoryRegistry()
	r := NewLayeredRegistry(primary, cache)

	r.Set("w1", "127.0.0.1:3838")

	if _, ok := primary.Get("w1"); !ok {
		t.Error("primary should have the entry")
	}
	if _, ok := cache.Get("w1"); !ok {
		t.Error("cache should have the entry")
	}
}

// TestLayeredRegistryDeletePropagates verifies Delete clears both
// layers — otherwise a cache hit could resurrect a deleted entry.
func TestLayeredRegistryDeletePropagates(t *testing.T) {
	primary := NewMemoryRegistry()
	cache := NewMemoryRegistry()
	r := NewLayeredRegistry(primary, cache)

	r.Set("w1", "127.0.0.1:3838")
	r.Delete("w1")

	if _, ok := primary.Get("w1"); ok {
		t.Error("primary should be empty after Delete")
	}
	if _, ok := cache.Get("w1"); ok {
		t.Error("cache should be empty after Delete")
	}
}

// TestLayeredRegistryGetMissBothLayers confirms Get returns false
// cleanly when neither layer has the entry.
func TestLayeredRegistryGetMissBothLayers(t *testing.T) {
	primary := NewMemoryRegistry()
	cache := NewMemoryRegistry()
	r := NewLayeredRegistry(primary, cache)

	if _, ok := r.Get("nonexistent"); ok {
		t.Error("Get should return false when both layers miss")
	}
}
