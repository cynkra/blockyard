package process

import (
	"fmt"
	"sync"
)

// uidAllocator manages a fixed range of host UIDs for workers. Each
// running worker is assigned a unique UID; on exit the UID is returned
// to the pool. The allocator is in-memory only — phase 3-8 (rolling
// updates) must size the UID range for ~2x peak workers, same as the
// port range, since both servers allocate from the same pool during
// the overlap window.
type uidAllocator struct {
	mu    sync.Mutex
	start int
	used  []bool // index = uid - start
}

func newUIDAllocator(start, end int) *uidAllocator {
	size := end - start + 1
	if size < 0 {
		size = 0
	}
	return &uidAllocator{
		start: start,
		used:  make([]bool, size),
	}
}

// Alloc returns the next free UID, or an error if all UIDs are in use.
func (u *uidAllocator) Alloc() (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, taken := range u.used {
		if !taken {
			u.used[i] = true
			return u.start + i, nil
		}
	}
	return 0, fmt.Errorf("process backend: all %d worker UIDs in use", len(u.used))
}

// Release returns a UID to the pool. No-op if out of range.
func (u *uidAllocator) Release(uid int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	idx := uid - u.start
	if idx >= 0 && idx < len(u.used) {
		u.used[idx] = false
	}
}

// InUse returns the number of currently allocated UIDs.
func (u *uidAllocator) InUse() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, taken := range u.used {
		if taken {
			n++
		}
	}
	return n
}
