package process

import (
	"context"
	"net"
	"sync"
	"testing"
)

func TestRedisPortAllocatorBasic(t *testing.T) {
	rc := newTestRedisClient(t, "test:")
	// Use a range that's unlikely to collide with anything on the
	// test host. 0 asks the kernel to pick a free ephemeral port
	// first — but we need a fixed range for the allocator, so
	// probe one dynamically and build a range around it.
	base := findFreePort(t)
	a := newRedisPortAllocator(rc, base, base+2, "host-a")

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

	// All distinct, all within range.
	if p1 == p2 || p2 == p3 || p1 == p3 {
		t.Errorf("duplicate ports: %d, %d, %d", p1, p2, p3)
	}
	for _, p := range []int{p1, p2, p3} {
		if p < base || p > base+2 {
			t.Errorf("port %d out of range [%d..%d]", p, base, base+2)
		}
	}

	// Fourth allocation fails.
	if _, _, err := a.Reserve(); err == nil {
		t.Error("expected error on exhausted range")
	}

	// Release + re-alloc.
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
}

// TestRedisPortAllocatorConcurrentDistinct — the atomicity guarantee.
func TestRedisPortAllocatorConcurrentDistinct(t *testing.T) {
	rc := newTestRedisClient(t, "test:")
	base := findFreePort(t)
	a := newRedisPortAllocator(rc, base, base+49, "host-a")

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
}

// TestRedisPortAllocatorKernelProbeSkipsExternallyBound covers the
// layered safety-net: Redis says a port is free but another process
// holds it on the host. The allocator must skip past the failed
// index and return a different port.
func TestRedisPortAllocatorKernelProbeSkipsExternallyBound(t *testing.T) {
	// Pre-bind a port before configuring the allocator's range
	// around it.
	pre, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pre.Close()
	boundPort := pre.Addr().(*net.TCPAddr).Port

	rc := newTestRedisClient(t, "test:")
	a := newRedisPortAllocator(rc, boundPort, boundPort+2, "host-a")

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
}

func TestRedisPortAllocatorPeerCoexistence(t *testing.T) {
	rc := newTestRedisClient(t, "test:")
	base := findFreePort(t)
	host1 := newRedisPortAllocator(rc, base, base+9, "host-1")
	host2 := newRedisPortAllocator(rc, base, base+9, "host-2")

	var held []net.Listener
	t.Cleanup(func() {
		for _, ln := range held {
			if ln != nil {
				ln.Close()
			}
		}
	})

	// host1 takes 5.
	for range 5 {
		_, ln, err := host1.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, ln)
	}
	// host2 takes 5.
	for range 5 {
		_, ln, err := host2.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, ln)
	}
	// 11th fails.
	if _, _, err := host1.Reserve(); err == nil {
		t.Error("expected exhaustion after 10 allocs across peers")
	}
}

// TestRedisPortAllocatorCleanupOwnedOrphans mirrors the UID cleanup
// test.
func TestRedisPortAllocatorCleanupOwnedOrphans(t *testing.T) {
	rc := newTestRedisClient(t, "test:")
	base := findFreePort(t)
	host1 := newRedisPortAllocator(rc, base, base+5, "host-1")
	host2 := newRedisPortAllocator(rc, base, base+5, "host-2")

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

	// Release host1's listeners so the kernel frees them. Cleanup
	// then returns host1's claims.
	for i, ln := range held[:3] {
		ln.Close()
		held[i] = nil
	}
	if err := host1.CleanupOwnedOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A fresh host1 can re-alloc its own 3 slots.
	fresh := newRedisPortAllocator(rc, base, base+5, "host-1")
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
}

// findFreePort asks the kernel for an ephemeral port and immediately
// closes the listener. Used to pick a base for allocator tests so
// CI runners don't collide with hardcoded ranges. The returned
// port may be reclaimed before the allocator binds it, but the
// probe loop inside the allocator handles that case via the
// skip_from mechanism.
func findFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

