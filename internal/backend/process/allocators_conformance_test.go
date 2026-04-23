package process

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/jmoiron/sqlx"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// portAllocatorFactory builds a port allocator for the given hostname
// and range. Implementations that share backing storage across hostnames
// (Redis, Postgres, Layered) take an *allocatorBackend that pins the
// shared resources (miniredis, *sqlx.DB) for the duration of the test.
type portAllocatorFactory func(t *testing.T, b *allocatorBackend, hostname string, first, last int) portAllocator

type uidAllocatorFactory func(t *testing.T, b *allocatorBackend, hostname string, first, last int) uidAllocator

// allocatorBackend lazily holds the shared backing resources for a
// test (one miniredis + one Postgres clone) so two allocators with
// different hostnames coordinate via the same storage. Each subtest
// only pays the bootstrap cost for the variants it actually uses —
// the Memory variant touches neither, Redis-only variants skip the
// PG clone, etc.
type allocatorBackend struct {
	t   *testing.T
	rc  *redisstate.Client
	rcM sync.Once
	pg  *sqlx.DB
	pgM sync.Once
}

func newAllocatorBackend(t *testing.T) *allocatorBackend {
	t.Helper()
	return &allocatorBackend{t: t}
}

func (b *allocatorBackend) redisClient() *redisstate.Client {
	b.rcM.Do(func() {
		mr := miniredis.RunT(b.t)
		b.rc = redisstate.TestClient(b.t, mr.Addr())
	})
	return b.rc
}

func (b *allocatorBackend) pgDB() *sqlx.DB {
	b.pgM.Do(func() {
		b.pg = testPGDB(b.t)
	})
	return b.pg
}

// portAllocatorImplementations lists every port-allocator backend.
// Memory-only callers can pick by name to skip peer-coexistence tests.
func portAllocatorImplementations() map[string]portAllocatorFactory {
	return map[string]portAllocatorFactory{
		"Memory": func(_ *testing.T, _ *allocatorBackend, _ string, first, last int) portAllocator {
			return newMemoryPortAllocator(first, last)
		},
		"Redis": func(t *testing.T, b *allocatorBackend, hostname string, first, last int) portAllocator {
			t.Helper()
			return newRedisPortAllocator(b.redisClient(), first, last, hostname)
		},
		"Postgres": func(t *testing.T, b *allocatorBackend, hostname string, first, last int) portAllocator {
			t.Helper()
			return newPostgresPortAllocator(b.pgDB(), first, last, hostname)
		},
		"Layered": func(t *testing.T, b *allocatorBackend, hostname string, first, last int) portAllocator {
			t.Helper()
			pg := newPostgresPortAllocator(b.pgDB(), first, last, hostname)
			rd := newRedisPortAllocator(b.redisClient(), first, last, hostname)
			return newLayeredPortAllocator(pg, rd)
		},
	}
}

func uidAllocatorImplementations() map[string]uidAllocatorFactory {
	return map[string]uidAllocatorFactory{
		"Memory": func(_ *testing.T, _ *allocatorBackend, _ string, first, last int) uidAllocator {
			return newMemoryUIDAllocator(first, last)
		},
		"Redis": func(t *testing.T, b *allocatorBackend, hostname string, first, last int) uidAllocator {
			t.Helper()
			return newRedisUIDAllocator(b.redisClient(), first, last, hostname)
		},
		"Postgres": func(t *testing.T, b *allocatorBackend, hostname string, first, last int) uidAllocator {
			t.Helper()
			return newPostgresUIDAllocator(b.pgDB(), first, last, hostname)
		},
		"Layered": func(t *testing.T, b *allocatorBackend, hostname string, first, last int) uidAllocator {
			t.Helper()
			pg := newPostgresUIDAllocator(b.pgDB(), first, last, hostname)
			rd := newRedisUIDAllocator(b.redisClient(), first, last, hostname)
			return newLayeredUIDAllocator(pg, rd)
		},
	}
}

// peerCoexistenceFactories returns only the implementations that share
// backing storage across hostnames. Memory has independent in-process
// bitsets per allocator instance and cannot coordinate.
func peerCoexistenceFactories[F any](impls map[string]F) map[string]F {
	out := make(map[string]F, len(impls))
	for name, f := range impls {
		if name == "Memory" {
			continue
		}
		out[name] = f
	}
	return out
}

// ── Port allocator conformance ──

func TestPortAllocatorConformance_BasicReserveRelease(t *testing.T) {
	for name, factory := range portAllocatorImplementations() {
		t.Run(name, func(t *testing.T) {
			b := newAllocatorBackend(t)
			base := findFreePortRange(t, 3)
			a := factory(t, b, "host-a", base, base+2)

			p1, ln1, err := a.Reserve()
			if err != nil {
				t.Fatal(err)
			}
			defer ln1.Close()
			p2, ln2, err := a.Reserve()
			if err != nil {
				t.Fatal(err)
			}
			defer ln2.Close()
			p3, ln3, err := a.Reserve()
			if err != nil {
				t.Fatal(err)
			}
			defer ln3.Close()

			if p1 == p2 || p2 == p3 || p1 == p3 {
				t.Errorf("duplicate ports: %d, %d, %d", p1, p2, p3)
			}
			for _, p := range []int{p1, p2, p3} {
				if p < base || p > base+2 {
					t.Errorf("port %d out of range [%d..%d]", p, base, base+2)
				}
			}

			if _, _, err := a.Reserve(); err == nil {
				t.Error("expected error on exhausted range")
			}

			ln2.Close()
			a.Release(p2)
			got, ln, err := a.Reserve()
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			if got != p2 {
				t.Errorf("re-alloc got %d, want %d", got, p2)
			}
		})
	}
}

func TestPortAllocatorConformance_ConcurrentDistinct(t *testing.T) {
	for name, factory := range portAllocatorImplementations() {
		t.Run(name, func(t *testing.T) {
			b := newAllocatorBackend(t)
			base := findFreePortRange(t, 50)
			a := factory(t, b, "host-a", base, base+49)

			var wg sync.WaitGroup
			type result struct {
				port int
				ln   net.Listener
			}
			results := make(chan result, 50)
			for range 50 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					p, ln, err := a.Reserve()
					if err == nil {
						results <- result{p, ln}
					}
				}()
			}
			wg.Wait()
			close(results)

			seen := make(map[int]bool)
			for r := range results {
				if seen[r.port] {
					t.Errorf("duplicate port: %d", r.port)
				}
				seen[r.port] = true
				r.ln.Close()
			}
			if len(seen) == 0 {
				t.Error("expected at least some successful reservations")
			}
		})
	}
}

func TestPortAllocatorConformance_KernelProbeSkipsExternallyBound(t *testing.T) {
	for name, factory := range portAllocatorImplementations() {
		t.Run(name, func(t *testing.T) {
			pre, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer pre.Close()
			boundPort := pre.Addr().(*net.TCPAddr).Port

			b := newAllocatorBackend(t)
			a := factory(t, b, "host-a", boundPort, boundPort+2)

			got, ln, err := a.Reserve()
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}
			defer ln.Close()
			if got == boundPort {
				t.Errorf("Reserve returned the externally-bound port %d", got)
			}
			if got < boundPort || got > boundPort+2 {
				t.Errorf("Reserve returned %d, outside range", got)
			}
		})
	}
}

func TestPortAllocatorConformance_PeerCoexistence(t *testing.T) {
	for name, factory := range peerCoexistenceFactories(portAllocatorImplementations()) {
		t.Run(name, func(t *testing.T) {
			b := newAllocatorBackend(t)
			base := findFreePortRange(t, 10)
			host1 := factory(t, b, "host-1", base, base+9)
			host2 := factory(t, b, "host-2", base, base+9)

			var held []net.Listener
			t.Cleanup(func() {
				for _, ln := range held {
					if ln != nil {
						ln.Close()
					}
				}
			})

			for range 5 {
				_, ln, err := host1.Reserve()
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, ln)
			}
			for range 5 {
				_, ln, err := host2.Reserve()
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, ln)
			}
			if _, _, err := host1.Reserve(); err == nil {
				t.Error("expected exhaustion after 10 allocs across peers")
			}
		})
	}
}

func TestPortAllocatorConformance_CleanupOwnedOrphans(t *testing.T) {
	for name, factory := range peerCoexistenceFactories(portAllocatorImplementations()) {
		t.Run(name, func(t *testing.T) {
			b := newAllocatorBackend(t)
			base := findFreePortRange(t, 6)
			host1 := factory(t, b, "host-1", base, base+5)
			host2 := factory(t, b, "host-2", base, base+5)

			cleaner1, ok1 := host1.(interface {
				CleanupOwnedOrphans(ctx context.Context) error
			})
			if !ok1 {
				t.Skip("backend does not support CleanupOwnedOrphans")
			}

			var held []net.Listener
			t.Cleanup(func() {
				for _, ln := range held {
					if ln != nil {
						ln.Close()
					}
				}
			})

			for range 3 {
				_, ln, err := host1.Reserve()
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, ln)
			}
			for range 3 {
				_, ln, err := host2.Reserve()
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, ln)
			}

			// Release host1's listeners so the kernel frees them; cleanup
			// then drops host1's claims from the shared store.
			for i, ln := range held[:3] {
				ln.Close()
				held[i] = nil
			}
			if err := cleaner1.CleanupOwnedOrphans(context.Background()); err != nil {
				t.Fatal(err)
			}

			fresh := factory(t, b, "host-1", base, base+5)
			for range 3 {
				_, ln, err := fresh.Reserve()
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, ln)
			}
			if _, _, err := fresh.Reserve(); err == nil {
				t.Error("expected exhaustion — host2's 3 entries should still be held")
			}
		})
	}
}

// ── UID allocator conformance ──

func TestUIDAllocatorConformance_BasicAllocRelease(t *testing.T) {
	for name, factory := range uidAllocatorImplementations() {
		t.Run(name, func(t *testing.T) {
			b := newAllocatorBackend(t)
			a := factory(t, b, "host-a", 60000, 60002)

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
				t.Error("expected exhaustion error")
			}
			a.Release(60001)
			next, err := a.Alloc()
			if err != nil {
				t.Fatal(err)
			}
			if next != 60001 {
				t.Errorf("re-allocated UID = %d, want 60001", next)
			}
		})
	}
}

func TestUIDAllocatorConformance_ConcurrentDistinct(t *testing.T) {
	for name, factory := range uidAllocatorImplementations() {
		t.Run(name, func(t *testing.T) {
			b := newAllocatorBackend(t)
			a := factory(t, b, "host-a", 60000, 60049)

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
					t.Errorf("duplicate UID: %d", uid)
				}
				seen[uid] = true
			}
			if len(seen) != 50 {
				t.Errorf("expected 50 unique UIDs, got %d", len(seen))
			}
		})
	}
}

func TestUIDAllocatorConformance_PeerCoexistence(t *testing.T) {
	for name, factory := range peerCoexistenceFactories(uidAllocatorImplementations()) {
		t.Run(name, func(t *testing.T) {
			b := newAllocatorBackend(t)
			host1 := factory(t, b, "host-1", 60000, 60009)
			host2 := factory(t, b, "host-2", 60000, 60009)

			var allocated []int
			for range 5 {
				u, err := host1.Alloc()
				if err != nil {
					t.Fatal(err)
				}
				allocated = append(allocated, u)
			}
			for range 5 {
				u, err := host2.Alloc()
				if err != nil {
					t.Fatal(err)
				}
				allocated = append(allocated, u)
			}
			if _, err := host1.Alloc(); err == nil {
				t.Error("expected exhaustion after 10 allocs across peers")
			}
			seen := make(map[int]bool)
			for _, u := range allocated {
				if seen[u] {
					t.Errorf("duplicate UID %d across peers", u)
				}
				seen[u] = true
			}
		})
	}
}

func TestUIDAllocatorConformance_ReleaseOwnership(t *testing.T) {
	for name, factory := range peerCoexistenceFactories(uidAllocatorImplementations()) {
		t.Run(name, func(t *testing.T) {
			b := newAllocatorBackend(t)
			host1 := factory(t, b, "host-1", 60000, 60002)
			host2 := factory(t, b, "host-2", 60000, 60002)

			u1, err := host1.Alloc()
			if err != nil {
				t.Fatal(err)
			}

			// host2 must not be able to release host1's UID.
			host2.Release(u1)
			u2, err := host1.Alloc()
			if err != nil {
				t.Fatal(err)
			}
			if u2 == u1 {
				t.Errorf("host2's release stole host1's UID %d", u1)
			}
		})
	}
}

func TestUIDAllocatorConformance_CleanupOwnedOrphans(t *testing.T) {
	for name, factory := range peerCoexistenceFactories(uidAllocatorImplementations()) {
		t.Run(name, func(t *testing.T) {
			b := newAllocatorBackend(t)
			host1 := factory(t, b, "host-1", 60000, 60005)
			host2 := factory(t, b, "host-2", 60000, 60005)

			cleaner1, ok1 := host1.(interface {
				CleanupOwnedOrphans(ctx context.Context) error
			})
			if !ok1 {
				t.Skip("backend does not support CleanupOwnedOrphans")
			}

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

			if err := cleaner1.CleanupOwnedOrphans(context.Background()); err != nil {
				t.Fatal(err)
			}

			fresh := factory(t, b, "host-1", 60000, 60005)
			for range 3 {
				if _, err := fresh.Alloc(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fresh.Alloc(); err == nil {
				t.Error("expected exhaustion — host2's 3 entries should still be held")
			}
		})
	}
}
