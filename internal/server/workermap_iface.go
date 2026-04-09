package server

import "time"

// WorkerMap defines the contract for the active worker map.
// MemoryWorkerMap is the in-process implementation; Redis implements
// the same interface for shared state during rolling updates.
type WorkerMap interface {
	Get(id string) (ActiveWorker, bool)
	Set(id string, w ActiveWorker)
	Delete(id string)
	Count() int
	CountForApp(appID string) int
	All() []string
	ForApp(appID string) []string
	ForAppAvailable(appID string) []string
	MarkDraining(appID string) []string
	SetDraining(workerID string)
	SetIdleSince(workerID string, t time.Time)
	SetIdleSinceIfZero(workerID string, t time.Time)
	ClearIdleSince(workerID string) bool
	IdleWorkers(timeout time.Duration) []string
	AppIDs() []string
	IsDraining(appID string) bool
	ClearDraining(workerID string)

	// WorkersForServer returns the worker IDs owned by the given
	// server_id. Used by Drainer.waitForIdle during a same-host
	// rolling update so the old server waits for its own sessions
	// to end, not the new server's fresh sessions. In the memory
	// implementation this is equivalent to All() — single-node
	// deployments have one server, so every worker belongs to it.
	// The Redis implementation filters by the server_id hash field
	// phase 3-3 already writes.
	WorkersForServer(serverID string) []string
}
