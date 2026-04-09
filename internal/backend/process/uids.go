package process

import (
	"context"
	"fmt"
	"sync"
)

// uidAllocator manages a fixed range of host UIDs for workers. Two
// implementations exist: memoryUIDAllocator (used when Redis is not
// configured; single-node only) and redisUIDAllocator (used when
// Redis is configured; coordinates across blockyard peers). Both
// share the same interface so the rest of the backend does not care
// which is live.
type uidAllocator interface {
	// Alloc returns the next free UID, or an error if all UIDs are
	// in use.
	Alloc() (int, error)

	// Release returns a UID to the pool. No-op if out of range.
	Release(uid int)

	// InUse returns the number of currently allocated UIDs.
	InUse() int
}

// memoryUIDAllocator is the in-memory bitset implementation.
type memoryUIDAllocator struct {
	mu    sync.Mutex
	start int
	used  []bool // index = uid - start
}

func newMemoryUIDAllocator(start, end int) *memoryUIDAllocator {
	size := end - start + 1
	if size < 0 {
		size = 0
	}
	return &memoryUIDAllocator{
		start: start,
		used:  make([]bool, size),
	}
}

// Alloc returns the next free UID, or an error if all UIDs are in use.
func (u *memoryUIDAllocator) Alloc() (int, error) {
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
func (u *memoryUIDAllocator) Release(uid int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	idx := uid - u.start
	if idx >= 0 && idx < len(u.used) {
		u.used[idx] = false
	}
}

// InUse returns the number of currently allocated UIDs.
func (u *memoryUIDAllocator) InUse() int {
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

// CleanupOwnedOrphans is a hook for the Redis-backed variant. For
// the memory variant this is a no-op — an in-memory bitset has
// nothing stale from a previous run.
func (u *memoryUIDAllocator) CleanupOwnedOrphans(_ context.Context) error {
	return nil
}
