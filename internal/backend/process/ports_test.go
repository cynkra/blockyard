package process

import (
	"sync"
	"testing"
)

func TestPortAllocator(t *testing.T) {
	p := newPortAllocator(40000, 40002)

	// Allocate all three ports.
	p1, err := p.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	p2, err := p.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	p3, err := p.Alloc()
	if err != nil {
		t.Fatal(err)
	}

	if p1 != 40000 || p2 != 40001 || p3 != 40002 {
		t.Errorf("expected 40000-40002, got %d, %d, %d", p1, p2, p3)
	}

	// Fourth allocation fails.
	if _, err := p.Alloc(); err == nil {
		t.Error("expected error when all ports in use")
	}

	// Release and re-allocate.
	p.Release(40001)
	got, err := p.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	if got != 40001 {
		t.Errorf("expected 40001 after release, got %d", got)
	}
}

func TestPortAllocatorConcurrent(t *testing.T) {
	p := newPortAllocator(40100, 40199)
	var wg sync.WaitGroup
	ports := make(chan int, 100)

	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port, err := p.Alloc()
			if err == nil {
				ports <- port
			}
		}()
	}
	wg.Wait()
	close(ports)

	seen := make(map[int]bool)
	for port := range ports {
		if seen[port] {
			t.Errorf("duplicate port allocation: %d", port)
		}
		seen[port] = true
	}
	// Note: probePort may fail for some ports if the host is busy. We
	// just verify there are no duplicates and at least some allocations
	// succeeded.
	if len(seen) == 0 {
		t.Error("expected at least one successful allocation")
	}
}

func TestPortAllocatorInUse(t *testing.T) {
	p := newPortAllocator(40300, 40302)
	if p.InUse() != 0 {
		t.Errorf("expected 0 in use, got %d", p.InUse())
	}
	p1, _ := p.Alloc()
	p2, _ := p.Alloc()
	if p.InUse() != 2 {
		t.Errorf("expected 2 in use, got %d", p.InUse())
	}
	p.Release(p1)
	if p.InUse() != 1 {
		t.Errorf("expected 1 in use, got %d", p.InUse())
	}
	p.Release(p2)
}
