package process

import (
	"fmt"
	"net"
	"sync"
	"testing"
)

// freeStartPort asks the OS for a free port, closes the listener, and
// returns that port number so it can seed a memoryPortAllocator range.
func freeStartPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func TestPortAllocator(t *testing.T) {
	start := freeStartPort(t)
	p := newMemoryPortAllocator(start, start+2)

	// Reserve all three ports.
	p1, ln1, err := p.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	defer ln1.Close()
	p2, ln2, err := p.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	p3, ln3, err := p.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	defer ln3.Close()

	if p1 != start || p2 != start+1 || p3 != start+2 {
		t.Errorf("expected %d-%d, got %d, %d, %d", start, start+2, p1, p2, p3)
	}

	// Fourth reservation fails.
	if _, _, err := p.Reserve(); err == nil {
		t.Error("expected error when all ports in use")
	}

	// Releasing the bitset slot is not enough on its own — the listener
	// for start+1 is still held, so the next Reserve must skip past it.
	// Close the listener first, then Release, then re-Reserve.
	ln2.Close()
	p.Release(start + 1)
	got, ln, err := p.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if got != start+1 {
		t.Errorf("expected %d after release, got %d", start+1, got)
	}
}

func TestPortAllocatorConcurrent(t *testing.T) {
	start := freeStartPort(t)
	p := newMemoryPortAllocator(start, start+99)
	var wg sync.WaitGroup
	type result struct {
		port int
		ln   net.Listener
	}
	results := make(chan result, 100)

	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port, ln, err := p.Reserve()
			if err == nil {
				results <- result{port, ln}
			}
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[int]bool)
	for r := range results {
		if seen[r.port] {
			t.Errorf("duplicate port reservation: %d", r.port)
		}
		seen[r.port] = true
		r.ln.Close()
	}
	// Note: Reserve may fail for some ports if the host is busy. We
	// just verify there are no duplicates and at least some reservations
	// succeeded.
	if len(seen) == 0 {
		t.Error("expected at least one successful reservation")
	}
}

func TestPortAllocatorInUse(t *testing.T) {
	start := freeStartPort(t)
	p := newMemoryPortAllocator(start, start+2)
	if p.InUse() != 0 {
		t.Errorf("expected 0 in use, got %d", p.InUse())
	}
	p1, ln1, _ := p.Reserve()
	defer ln1.Close()
	p2, ln2, _ := p.Reserve()
	defer ln2.Close()
	if p.InUse() != 2 {
		t.Errorf("expected 2 in use, got %d", p.InUse())
	}
	p.Release(p1)
	if p.InUse() != 1 {
		t.Errorf("expected 1 in use, got %d", p.InUse())
	}
	p.Release(p2)
}

// TestPortAllocatorSkipsExternallyBoundPort covers the TOCTOU
// recovery path: if another process already bound a port in the
// range, Reserve must listen-fail past it to the next free slot
// rather than hand out a dud port.
func TestPortAllocatorSkipsExternallyBoundPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	boundPort := ln.Addr().(*net.TCPAddr).Port

	p := newMemoryPortAllocator(boundPort, boundPort+2)
	got, gotLn, err := p.Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	defer gotLn.Close()
	if got == boundPort {
		t.Errorf("Reserve handed back the externally-bound port %d", got)
	}
	if got < boundPort || got > boundPort+2 {
		t.Errorf("Reserve returned %d, outside range [%d..%d]", got, boundPort, boundPort+2)
	}
}

// TestPortAllocatorReserveHoldsListener verifies the core property
// the new API exists for: a Reserve'd port cannot be bound by another
// process until the caller closes the returned listener.
func TestPortAllocatorReserveHoldsListener(t *testing.T) {
	start := freeStartPort(t)
	p := newMemoryPortAllocator(start, start+2)
	port, ln, err := p.Reserve()
	if err != nil {
		t.Fatal(err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Another listen on the same port must fail while ln is held.
	other, err := net.Listen("tcp", addr)
	if err == nil {
		other.Close()
		t.Errorf("expected listen on held port %d to fail, but it succeeded", port)
	}

	// After closing, the port becomes bindable again.
	ln.Close()
	other, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen on released port %d failed: %v", port, err)
	}
	other.Close()
}

// TestNewPortAllocatorDefensiveNegativeRange guards against end<start
// panicking `make([]bool, -N)`. The defensive clamp produces an
// empty pool whose Reserve always errors.
func TestNewPortAllocatorDefensiveNegativeRange(t *testing.T) {
	p := newMemoryPortAllocator(10, 5)
	if len(p.used) != 0 {
		t.Errorf("expected empty used slice, got len=%d", len(p.used))
	}
	if _, _, err := p.Reserve(); err == nil {
		t.Error("expected Reserve error on empty range")
	}
}

func TestPortAllocatorReleaseOutOfRange(t *testing.T) {
	start := freeStartPort(t)
	p := newMemoryPortAllocator(start, start+2)
	p.Release(0)     // below range
	p.Release(99999) // above range
	if p.InUse() != 0 {
		t.Errorf("InUse changed after out-of-range releases: %d", p.InUse())
	}
}

