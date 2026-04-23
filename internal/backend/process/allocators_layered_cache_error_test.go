package process

import (
	"context"
	"net"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// Cache-failure behavior for the layered port / UID allocators
// (see #262, #288). Postgres is the cross-peer mutex; Redis is a
// best-effort mirror. These tests pair a real Postgres primary with a
// real Redis cache whose miniredis is poisoned via SetError, and
// assert that Reserve / Alloc / Release / CleanupOwnedOrphans all
// complete without surfacing the cache failure to the caller.

func newLayeredPortAllocatorWithErroringCache(t *testing.T, first, last int) (*layeredPortAllocator, *miniredis.Miniredis) {
	t.Helper()
	db := testPGDB(t)
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	pg := newPostgresPortAllocator(db, first, last, "host-a")
	rd := newRedisPortAllocator(client, first, last, "host-a")
	return newLayeredPortAllocator(pg, rd), mr
}

func newLayeredUIDAllocatorWithErroringCache(t *testing.T, first, last int) (*layeredUIDAllocator, *miniredis.Miniredis) {
	t.Helper()
	db := testPGDB(t)
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	pg := newPostgresUIDAllocator(db, first, last, "host-a")
	rd := newRedisUIDAllocator(client, first, last, "host-a")
	return newLayeredUIDAllocator(pg, rd), mr
}

func TestLayeredPortAllocator_CacheErrors_ReserveSucceeds(t *testing.T) {
	base := findFreePortRange(t, 3)
	a, mr := newLayeredPortAllocatorWithErroringCache(t, base, base+2)

	mr.SetError("READONLY simulated failure")

	// Postgres still arbitrates ownership; the cache mirror just logs.
	port, ln, err := a.Reserve()
	if err != nil {
		t.Fatalf("Reserve with cache errors: %v", err)
	}
	defer ln.Close()
	if port < base || port > base+2 {
		t.Errorf("port %d out of range [%d..%d]", port, base, base+2)
	}
}

func TestLayeredPortAllocator_CacheErrors_ReleaseSucceeds(t *testing.T) {
	base := findFreePortRange(t, 3)
	a, mr := newLayeredPortAllocatorWithErroringCache(t, base, base+2)

	port, ln, err := a.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	mr.SetError("READONLY simulated failure")
	a.Release(port) // must not panic or block

	// Primary must be cleared regardless of cache error — a subsequent
	// Reserve should be able to hand the port back out.
	mr.SetError("") // clear to let Reserve's mirror succeed
	var freshLns []net.Listener
	defer func() {
		for _, ln := range freshLns {
			ln.Close()
		}
	}()
	for range 3 {
		_, fresh, err := a.Reserve()
		if err != nil {
			t.Fatalf("unexpected exhaustion after release with cache errors: %v", err)
		}
		freshLns = append(freshLns, fresh)
	}
}

func TestLayeredPortAllocator_CacheErrors_CleanupIsBestEffort(t *testing.T) {
	base := findFreePortRange(t, 3)
	a, mr := newLayeredPortAllocatorWithErroringCache(t, base, base+2)

	mr.SetError("READONLY simulated failure")

	// Cleanup at startup must not fail when Redis is unreachable —
	// otherwise a Redis outage blocks recovery of orphaned Postgres rows.
	if err := a.CleanupOwnedOrphans(context.Background()); err != nil {
		t.Errorf("CleanupOwnedOrphans must tolerate cache errors, got: %v", err)
	}
}

func TestLayeredUIDAllocator_CacheErrors_AllocSucceeds(t *testing.T) {
	a, mr := newLayeredUIDAllocatorWithErroringCache(t, 60000, 60002)

	mr.SetError("READONLY simulated failure")

	uid, err := a.Alloc()
	if err != nil {
		t.Fatalf("Alloc with cache errors: %v", err)
	}
	if uid < 60000 || uid > 60002 {
		t.Errorf("uid %d out of range [60000..60002]", uid)
	}
}

func TestLayeredUIDAllocator_CacheErrors_ReleaseSucceeds(t *testing.T) {
	a, mr := newLayeredUIDAllocatorWithErroringCache(t, 60000, 60002)

	uid, err := a.Alloc()
	if err != nil {
		t.Fatal(err)
	}

	mr.SetError("READONLY simulated failure")
	a.Release(uid) // must not panic

	// Primary should have released the UID; after clearing the cache
	// error, re-Alloc should succeed without surfacing a false exhaustion.
	mr.SetError("")
	if _, err := a.Alloc(); err != nil {
		t.Fatalf("Alloc after release-with-errors: %v", err)
	}
	if _, err := a.Alloc(); err != nil {
		t.Fatalf("second Alloc: %v", err)
	}
}

func TestLayeredUIDAllocator_CacheErrors_CleanupIsBestEffort(t *testing.T) {
	a, mr := newLayeredUIDAllocatorWithErroringCache(t, 60000, 60002)

	mr.SetError("READONLY simulated failure")

	if err := a.CleanupOwnedOrphans(context.Background()); err != nil {
		t.Errorf("CleanupOwnedOrphans must tolerate cache errors, got: %v", err)
	}
}
