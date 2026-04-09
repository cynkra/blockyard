package process

import (
	"sync"
	"testing"
)

func TestUIDAllocator(t *testing.T) {
	u := newMemoryUIDAllocator(60000, 60002)

	u1, err := u.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	u2, err := u.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	u3, err := u.Alloc()
	if err != nil {
		t.Fatal(err)
	}

	if u1 != 60000 || u2 != 60001 || u3 != 60002 {
		t.Errorf("expected 60000-60002, got %d, %d, %d", u1, u2, u3)
	}

	if _, err := u.Alloc(); err == nil {
		t.Error("expected error when all UIDs in use")
	}

	u.Release(60001)
	got, err := u.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	if got != 60001 {
		t.Errorf("expected 60001 after release, got %d", got)
	}
}

func TestUIDAllocatorConcurrent(t *testing.T) {
	u := newMemoryUIDAllocator(60100, 60199)
	var wg sync.WaitGroup
	uids := make(chan int, 100)

	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			uid, err := u.Alloc()
			if err == nil {
				uids <- uid
			}
		}()
	}
	wg.Wait()
	close(uids)

	seen := make(map[int]bool)
	for uid := range uids {
		if seen[uid] {
			t.Errorf("duplicate UID allocation: %d", uid)
		}
		seen[uid] = true
	}
	if len(seen) != 100 {
		t.Errorf("expected 100 unique UIDs, got %d", len(seen))
	}
}

func TestUIDAllocatorReleaseOutOfRange(t *testing.T) {
	u := newMemoryUIDAllocator(60000, 60010)
	// Out-of-range release is a no-op.
	u.Release(0)
	u.Release(99999)
	if _, err := u.Alloc(); err != nil {
		t.Errorf("Alloc after out-of-range Release: %v", err)
	}
}

// TestNewUIDAllocatorDefensiveNegativeRange verifies the constructor
// clamps end < start to an empty pool instead of allocating a huge
// slice with a negative length (which would panic in make).
func TestNewUIDAllocatorDefensiveNegativeRange(t *testing.T) {
	u := newMemoryUIDAllocator(100, 5)
	if len(u.used) != 0 {
		t.Errorf("expected empty used slice, got len=%d", len(u.used))
	}
	if _, err := u.Alloc(); err == nil {
		t.Error("expected Alloc error on empty range")
	}
}
