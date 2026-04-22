package session

import "time"

// LayeredStore layers a cache Store over a primary Store (see #286,
// parent #262). The primary is the source of truth (Postgres in
// production); the cache is an optional optimization (Redis).
//
// Reads: cache first; on miss, fall back to primary and populate the
// cache on the way out.
//
// Writes: primary first; cache mirrored best-effort. Cache errors are
// already swallowed inside the concrete stores (they log and return
// false/zero), so LayeredStore just calls both sequentially — the
// primary operation's outcome is the one surfaced to callers.
//
// Aggregate queries (CountForWorker, EntriesForWorker) always go to
// the primary: the cache may hold a subset and can't answer accurately.
type LayeredStore struct {
	primary Store
	cache   Store
}

func NewLayeredStore(primary, cache Store) *LayeredStore {
	return &LayeredStore{primary: primary, cache: cache}
}

func (s *LayeredStore) Get(sessionID string) (Entry, bool) {
	if e, ok := s.cache.Get(sessionID); ok {
		return e, true
	}
	e, ok := s.primary.Get(sessionID)
	if ok {
		s.cache.Set(sessionID, e)
	}
	return e, ok
}

func (s *LayeredStore) Set(sessionID string, entry Entry) {
	s.primary.Set(sessionID, entry)
	s.cache.Set(sessionID, entry)
}

func (s *LayeredStore) Touch(sessionID string) bool {
	ok := s.primary.Touch(sessionID)
	if ok {
		s.cache.Touch(sessionID)
	}
	return ok
}

func (s *LayeredStore) Delete(sessionID string) {
	s.primary.Delete(sessionID)
	s.cache.Delete(sessionID)
}

func (s *LayeredStore) DeleteByWorker(workerID string) int {
	n := s.primary.DeleteByWorker(workerID)
	s.cache.DeleteByWorker(workerID)
	return n
}

func (s *LayeredStore) CountForWorker(workerID string) int {
	return s.primary.CountForWorker(workerID)
}

func (s *LayeredStore) CountForWorkers(workerIDs []string) int {
	return s.primary.CountForWorkers(workerIDs)
}

func (s *LayeredStore) RerouteWorker(oldWorkerID, newWorkerID string) int {
	n := s.primary.RerouteWorker(oldWorkerID, newWorkerID)
	s.cache.RerouteWorker(oldWorkerID, newWorkerID)
	return n
}

func (s *LayeredStore) EntriesForWorker(workerID string) map[string]Entry {
	return s.primary.EntriesForWorker(workerID)
}

// SweepIdle runs against the primary. RedisStore.SweepIdle is a no-op
// because native TTL already expires idle cache entries around the
// same cutoff — idle sweep and TTL share the idle_ttl setting.
func (s *LayeredStore) SweepIdle(maxAge time.Duration) int {
	n := s.primary.SweepIdle(maxAge)
	s.cache.SweepIdle(maxAge)
	return n
}

var _ Store = (*LayeredStore)(nil)
