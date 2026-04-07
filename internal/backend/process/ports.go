package process

import (
	"fmt"
	"net"
	"sync"
)

// portAllocator manages a fixed range of localhost ports for workers.
// Allocations are O(n) in the range size; the range is small (~1000)
// and Spawn is infrequent so the linear scan is acceptable.
type portAllocator struct {
	mu    sync.Mutex
	start int
	used  []bool // index = port - start
}

func newPortAllocator(start, end int) *portAllocator {
	size := end - start + 1
	if size < 0 {
		size = 0
	}
	return &portAllocator{
		start: start,
		used:  make([]bool, size),
	}
}

// Alloc returns the next free port, or an error if all ports are in use.
// After marking a port as allocated in the bitset, it verifies the port
// is actually bindable (TCP listen + immediate close). This prevents
// TOCTOU failures where another process on the host has already bound
// the port. If the probe fails, the port is skipped and the scan
// continues to the next free slot.
func (p *portAllocator) Alloc() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, taken := range p.used {
		if taken {
			continue
		}
		port := p.start + i
		if !probePort(port) {
			continue // port in use by another process; skip
		}
		p.used[i] = true
		return port, nil
	}
	return 0, fmt.Errorf("process backend: all %d ports in use", len(p.used))
}

// probePort attempts a TCP listen on 127.0.0.1:port to verify the port
// is available. Returns true if the listen succeeds (port is free).
func probePort(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// Release returns a port to the pool. No-op if the port is out of range.
func (p *portAllocator) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := port - p.start
	if idx >= 0 && idx < len(p.used) {
		p.used[idx] = false
	}
}

// InUse returns the number of currently allocated ports.
func (p *portAllocator) InUse() int {
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
