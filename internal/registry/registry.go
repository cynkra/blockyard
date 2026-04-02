package registry

import "sync"

// MemoryRegistry is a concurrent in-memory implementation of WorkerRegistry.
type MemoryRegistry struct {
	mu    sync.Mutex
	addrs map[string]string // worker ID → "host:port"
}

func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{addrs: make(map[string]string)}
}

func (r *MemoryRegistry) Get(workerID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	addr, ok := r.addrs[workerID]
	return addr, ok
}

func (r *MemoryRegistry) Set(workerID, addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addrs[workerID] = addr
}

func (r *MemoryRegistry) Delete(workerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.addrs, workerID)
}

var _ WorkerRegistry = (*MemoryRegistry)(nil)
