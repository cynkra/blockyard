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
}
