package server

import "sync"

// WsConnCounter tracks active WebSocket connections per worker.
//
// Used to enforce max_sessions_per_worker at WebSocket handshake time
// and to inform load-balancer assignment and idle detection. A session
// in blockyard is one active Shiny WebSocket — one browser tab —
// not one browser cookie, so this counter is what "max sessions" gates
// against. The session.Store continues to track cookie-level worker
// pinning for HTTP routing stickiness; it does not gate capacity.
type WsConnCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func NewWsConnCounter() *WsConnCounter {
	return &WsConnCounter{counts: make(map[string]int)}
}

// TryInc atomically increments the count for workerID if it is strictly
// below max. Returns true on success, false if the worker is already at
// capacity. A single mutex-protected check-and-increment avoids the race
// where two concurrent handshakes both observe count < max.
func (c *WsConnCounter) TryInc(workerID string, max int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[workerID] >= max {
		return false
	}
	c.counts[workerID]++
	return true
}

// Dec decrements the count for workerID. Idempotent at zero.
func (c *WsConnCounter) Dec(workerID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[workerID] > 0 {
		c.counts[workerID]--
	}
	if c.counts[workerID] == 0 {
		delete(c.counts, workerID)
	}
}

// Count returns the current active WS count for workerID.
func (c *WsConnCounter) Count(workerID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[workerID]
}

// CountForWorkers sums active WS counts across the given worker IDs.
func (c *WsConnCounter) CountForWorkers(ids []string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, id := range ids {
		n += c.counts[id]
	}
	return n
}

// DeleteWorker drops the entry for workerID. Called on eviction so
// stale counts don't linger against a vanished worker.
func (c *WsConnCounter) DeleteWorker(workerID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.counts, workerID)
}
