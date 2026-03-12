package registry

import "sync"

type Registry struct {
	mu    sync.Mutex
	addrs map[string]string // worker ID → "host:port"
}

func New() *Registry {
	return &Registry{addrs: make(map[string]string)}
}

func (r *Registry) Get(workerID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	addr, ok := r.addrs[workerID]
	return addr, ok
}

func (r *Registry) Set(workerID, addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addrs[workerID] = addr
}

func (r *Registry) Delete(workerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.addrs, workerID)
}
