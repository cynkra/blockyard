package process

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
)

func TestRedisPortAllocatorBasic(t *testing.T) {
	rc := newTestRedisClient(t, "test:")
	// Probe a contiguous block wide enough for the allocator range
	// so we're not flaky when the ephemeral pool is crowded.
	base := findFreePortRange(t, 3)
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
	base := findFreePortRange(t, 50)
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
	base := findFreePortRange(t, 10)
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
	base := findFreePortRange(t, 6)
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

// findFreePortRange returns the base of an n-port contiguous block
// that is entirely free on 127.0.0.1 right now. Used by tests that
// exercise the allocator across the whole range: those tests fail
// with "no free ports in range" if even a single port in the window
// is held by another process on the runner.
//
// Probe once per attempt: pick an ephemeral base, try to bind every
// port in [base, base+n). If all n succeed, close them all and
// return the base — there is still a small TOCTOU window before the
// test's Reserve calls grab them, but it is vastly smaller than the
// old "probe one, hope five more are free" pattern.
//
// The per-attempt retry loop exists because on busy CI runners the
// first ephemeral base may land next to something noisy; a few
// retries reliably find a clean block.
func findFreePortRange(t *testing.T, n int) int {
	t.Helper()
	const maxAttempts = 20
	for attempt := 0; attempt < maxAttempts; attempt++ {
		probe, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		base := probe.Addr().(*net.TCPAddr).Port
		probe.Close()

		lns := make([]net.Listener, 0, n)
		ok := true
		for i := 0; i < n; i++ {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				ok = false
				break
			}
			lns = append(lns, ln)
		}
		for _, ln := range lns {
			ln.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatalf("could not find %d contiguous free ports after %d attempts", n, maxAttempts)
	return 0
}

