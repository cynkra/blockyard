package registry

// LayeredRegistry layers a cache WorkerRegistry over a primary (see
// #287, parent #262). The primary is the source of truth (Postgres
// in production); the cache is an optional optimization (Redis).
//
// Reads: cache first; on miss, fall back to primary and populate the
// cache on the way out.
//
// Writes: primary first; cache mirrored best-effort. Cache errors are
// swallowed inside the concrete stores, so LayeredRegistry just calls
// both — the primary operation's outcome is the one surfaced.
type LayeredRegistry struct {
	primary WorkerRegistry
	cache   WorkerRegistry
}

func NewLayeredRegistry(primary, cache WorkerRegistry) *LayeredRegistry {
	return &LayeredRegistry{primary: primary, cache: cache}
}

func (r *LayeredRegistry) Get(workerID string) (string, bool) {
	if addr, ok := r.cache.Get(workerID); ok {
		return addr, true
	}
	addr, ok := r.primary.Get(workerID)
	if ok {
		r.cache.Set(workerID, addr)
	}
	return addr, ok
}

func (r *LayeredRegistry) Set(workerID, addr string) {
	r.primary.Set(workerID, addr)
	r.cache.Set(workerID, addr)
}

func (r *LayeredRegistry) Delete(workerID string) {
	r.primary.Delete(workerID)
	r.cache.Delete(workerID)
}

var _ WorkerRegistry = (*LayeredRegistry)(nil)
