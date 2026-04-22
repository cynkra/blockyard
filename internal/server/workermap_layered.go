package server

import "time"

// LayeredWorkerMap layers a cache WorkerMap over a primary (see #287,
// parent #262). The primary is the source of truth (Postgres in
// production); the cache is an optional optimization (Redis).
//
// Reads: cache first; on miss, fall back to primary and populate the
// cache on the way out.
//
// Writes: primary first; cache mirrored best-effort. Cache errors are
// swallowed inside the concrete stores, so LayeredWorkerMap just calls
// both — the primary operation's outcome is the one surfaced.
//
// Aggregate queries (Count, CountForApp, ForApp, MarkDraining, …)
// always go to the primary: the cache may hold a subset and can't
// answer accurately.
type LayeredWorkerMap struct {
	primary WorkerMap
	cache   WorkerMap
}

func NewLayeredWorkerMap(primary, cache WorkerMap) *LayeredWorkerMap {
	return &LayeredWorkerMap{primary: primary, cache: cache}
}

func (m *LayeredWorkerMap) Get(id string) (ActiveWorker, bool) {
	if w, ok := m.cache.Get(id); ok {
		return w, true
	}
	w, ok := m.primary.Get(id)
	if ok {
		m.cache.Set(id, w)
	}
	return w, ok
}

func (m *LayeredWorkerMap) Set(id string, w ActiveWorker) {
	m.primary.Set(id, w)
	m.cache.Set(id, w)
}

func (m *LayeredWorkerMap) Delete(id string) {
	m.primary.Delete(id)
	m.cache.Delete(id)
}

func (m *LayeredWorkerMap) Count() int                 { return m.primary.Count() }
func (m *LayeredWorkerMap) CountForApp(appID string) int { return m.primary.CountForApp(appID) }
func (m *LayeredWorkerMap) All() []string              { return m.primary.All() }
func (m *LayeredWorkerMap) ForApp(appID string) []string {
	return m.primary.ForApp(appID)
}
func (m *LayeredWorkerMap) ForAppAvailable(appID string) []string {
	return m.primary.ForAppAvailable(appID)
}

// MarkDraining writes the drain flag to the primary, then mirrors the
// state to the cache for each affected worker so a subsequent cache
// hit returns the new draining flag.
func (m *LayeredWorkerMap) MarkDraining(appID string) []string {
	ids := m.primary.MarkDraining(appID)
	for _, id := range ids {
		m.cache.SetDraining(id)
	}
	return ids
}

func (m *LayeredWorkerMap) SetDraining(workerID string) {
	m.primary.SetDraining(workerID)
	m.cache.SetDraining(workerID)
}

func (m *LayeredWorkerMap) ClearDraining(workerID string) {
	m.primary.ClearDraining(workerID)
	m.cache.ClearDraining(workerID)
}

func (m *LayeredWorkerMap) SetIdleSince(workerID string, t time.Time) {
	m.primary.SetIdleSince(workerID, t)
	m.cache.SetIdleSince(workerID, t)
}

func (m *LayeredWorkerMap) SetIdleSinceIfZero(workerID string, t time.Time) {
	m.primary.SetIdleSinceIfZero(workerID, t)
	m.cache.SetIdleSinceIfZero(workerID, t)
}

func (m *LayeredWorkerMap) ClearIdleSince(workerID string) bool {
	ok := m.primary.ClearIdleSince(workerID)
	if ok {
		m.cache.ClearIdleSince(workerID)
	}
	return ok
}

func (m *LayeredWorkerMap) IdleWorkers(timeout time.Duration) []string {
	return m.primary.IdleWorkers(timeout)
}

func (m *LayeredWorkerMap) AppIDs() []string { return m.primary.AppIDs() }

func (m *LayeredWorkerMap) IsDraining(appID string) bool {
	return m.primary.IsDraining(appID)
}

func (m *LayeredWorkerMap) WorkersForServer(serverID string) []string {
	return m.primary.WorkersForServer(serverID)
}

var _ WorkerMap = (*LayeredWorkerMap)(nil)
