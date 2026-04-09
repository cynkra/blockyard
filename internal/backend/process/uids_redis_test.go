package process

import (
	"context"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/redisstate"
)

// newTestRedisClient starts a miniredis instance and returns a
// redisstate.Client connected to it, along with a cleanup hook.
func newTestRedisClient(t *testing.T, keyPrefix string) *redisstate.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rc, err := redisstate.New(context.Background(), &config.RedisConfig{
		URL:       "redis://" + mr.Addr(),
		KeyPrefix: keyPrefix,
	})
	if err != nil {
		t.Fatalf("redisstate.New: %v", err)
	}
	t.Cleanup(func() { rc.Close() })
	return rc
}

func TestRedisUIDAllocatorBasic(t *testing.T) {
	rc := newTestRedisClient(t, "test:")
	a := newRedisUIDAllocator(rc, 60000, 60002, "host-a")

	u1, err := a.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	u2, err := a.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	u3, err := a.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	if u1 != 60000 || u2 != 60001 || u3 != 60002 {
		t.Errorf("allocated UIDs = %d, %d, %d; want 60000, 60001, 60002", u1, u2, u3)
	}
	if _, err := a.Alloc(); err == nil {
		t.Error("expected error when range exhausted")
	}
	a.Release(60001)
	next, err := a.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	if next != 60001 {
		t.Errorf("re-allocated UID = %d, want 60001", next)
	}
}

// TestRedisUIDAllocatorConcurrentDistinct is the load-bearing
// correctness check: if two goroutines Alloc in parallel, they must
// never return the same UID. The Lua script is the atomicity
// guarantee.
func TestRedisUIDAllocatorConcurrentDistinct(t *testing.T) {
	rc := newTestRedisClient(t, "test:")
	a := newRedisUIDAllocator(rc, 60000, 60049, "host-a")

	var wg sync.WaitGroup
	results := make(chan int, 50)
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			uid, err := a.Alloc()
			if err == nil {
				results <- uid
			}
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[int]bool)
	for uid := range results {
		if seen[uid] {
			t.Errorf("duplicate UID allocation: %d", uid)
		}
		seen[uid] = true
	}
	if len(seen) != 50 {
		t.Errorf("expected 50 unique UIDs, got %d", len(seen))
	}
}

// TestRedisUIDAllocatorPeerCoexistence verifies that two
// instances with different hostnames coordinate via Redis.
// Simulates the rolling-update overlap: old and new servers both
// alloc from the same range and must never collide.
func TestRedisUIDAllocatorPeerCoexistence(t *testing.T) {
	rc := newTestRedisClient(t, "test:")
	host1 := newRedisUIDAllocator(rc, 60000, 60009, "host-1")
	host2 := newRedisUIDAllocator(rc, 60000, 60009, "host-2")

	// host1 takes 5, host2 takes 5 — the 11th alloc (either peer) fails.
	var uids []int
	for range 5 {
		u, err := host1.Alloc()
		if err != nil {
			t.Fatal(err)
		}
		uids = append(uids, u)
	}
	for range 5 {
		u, err := host2.Alloc()
		if err != nil {
			t.Fatal(err)
		}
		uids = append(uids, u)
	}
	// Any next alloc fails.
	if _, err := host1.Alloc(); err == nil {
		t.Error("expected exhaustion after 10 allocs across peers")
	}
	// Verify all 10 UIDs are distinct.
	seen := make(map[int]bool)
	for _, u := range uids {
		if seen[u] {
			t.Errorf("duplicate UID %d across peers", u)
		}
		seen[u] = true
	}
}

func TestRedisUIDAllocatorReleaseOwnership(t *testing.T) {
	rc := newTestRedisClient(t, "test:")
	host1 := newRedisUIDAllocator(rc, 60000, 60002, "host-1")
	host2 := newRedisUIDAllocator(rc, 60000, 60002, "host-2")

	u1, err := host1.Alloc()
	if err != nil {
		t.Fatal(err)
	}

	// host2 cannot release host1's UID.
	host2.Release(u1)
	// host1 can still re-alloc it... no wait, it's still claimed.
	// Verify by trying to alloc it from host1's own Alloc().
	u2, err := host1.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	if u2 == u1 {
		t.Errorf("host2's release stole host1's UID %d", u1)
	}
}

// TestRedisUIDAllocatorCleanupOwnedOrphans populates Redis with
// entries owned by multiple hosts, then verifies cleanup only
// deletes entries owned by the local hostname.
func TestRedisUIDAllocatorCleanupOwnedOrphans(t *testing.T) {
	rc := newTestRedisClient(t, "test:")
	host1 := newRedisUIDAllocator(rc, 60000, 60005, "host-1")
	host2 := newRedisUIDAllocator(rc, 60000, 60005, "host-2")

	// host1 claims 3, host2 claims 3.
	for range 3 {
		if _, err := host1.Alloc(); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if _, err := host2.Alloc(); err != nil {
			t.Fatal(err)
		}
	}

	// host1 "restarts" and cleans up its orphans.
	if err := host1.CleanupOwnedOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}

	// After cleanup, host1 should be able to re-alloc 3 UIDs (from
	// the slots it previously held) but no more.
	fresh := newRedisUIDAllocator(rc, 60000, 60005, "host-1")
	var uids []int
	for range 3 {
		u, err := fresh.Alloc()
		if err != nil {
			t.Fatal(err)
		}
		uids = append(uids, u)
	}
	if _, err := fresh.Alloc(); err == nil {
		t.Error("expected exhaustion — host2's 3 entries should still be held")
	}
}
