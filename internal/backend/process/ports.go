package process

import (
	"context"
	"fmt"
	"net"
	"sync"
)

// portAllocator manages a fixed range of localhost ports for workers.
// Two implementations exist: memoryPortAllocator (used when Redis is
// not configured; single-node only) and redisPortAllocator (used when
// Redis is configured; coordinates across blockyard peers during
// rolling-update overlap). Both share the same interface so the rest
// of the backend does not care which is live.
type portAllocator interface {
	// Reserve picks a free port, holds a listener on it, and returns
	// (port, listener, nil). The caller MUST close the listener
	// immediately before cmd.Start (the issue #173 pattern) and call
	// Release on the port when the worker exits.
	Reserve() (port int, ln net.Listener, err error)

	// Release returns a port to the pool. No-op for out-of-range ports.
	Release(port int)

	// InUse returns the number of currently allocated ports.
	InUse() int
}

// memoryPortAllocator is the in-memory bitset implementation.
// Allocations are O(n) in the range size; the range is small (~1000)
// and Spawn is infrequent so the linear scan is acceptable.
type memoryPortAllocator struct {
	mu    sync.Mutex
	start int
	used  []bool // index = port - start
}

func newMemoryPortAllocator(start, end int) *memoryPortAllocator {
	size := end - start + 1
	if size < 0 {
		size = 0
	}
	return &memoryPortAllocator{
		start: start,
		used:  make([]bool, size),
	}
}

// Reserve picks a free port, holds a listener on it, and returns
// (port, listener, nil). The caller MUST close the listener immediately
// before invoking the child process that will bind the port — holding
// the listener prevents any other host process from grabbing the port
// during Spawn's setup work, shrinking the TOCTOU window from "Spawn
// setup latency" (~ms) to "the gap between ln.Close() and cmd.Start()"
// (~µs).
//
// If a candidate port cannot be bound (another host process already
// holds it), the slot is skipped and the scan continues. The bitset
// entry is set before returning so concurrent Reserves do not collide
// on the same slot; Release returns the slot to the pool.
func (p *memoryPortAllocator) Reserve() (int, net.Listener, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, taken := range p.used {
		if taken {
			continue
		}
		port := p.start + i
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue // port in use by another host process; skip
		}
		p.used[i] = true
		return port, ln, nil
	}
	return 0, nil, fmt.Errorf("process backend: all %d ports in use", len(p.used))
}

// Release returns a port to the pool. No-op if the port is out of range.
func (p *memoryPortAllocator) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := port - p.start
	if idx >= 0 && idx < len(p.used) {
		p.used[idx] = false
	}
}

// InUse returns the number of currently allocated ports.
func (p *memoryPortAllocator) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, taken := range p.used {
		if taken {
			n++
		}
	}
	return n
}

// cleanupPortOrphans is a hook for the Redis-backed variant. For the
// memory variant this is a no-op — an in-memory bitset has nothing
// stale from a previous run.
func (p *memoryPortAllocator) CleanupOwnedOrphans(_ context.Context) error {
	return nil
}
