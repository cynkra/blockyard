package session

import "time"

// Store defines the contract for session state storage.
// MemoryStore is the in-process implementation; Redis implements
// the same interface for shared state during rolling updates.
type Store interface {
	Get(sessionID string) (Entry, bool)
	Set(sessionID string, entry Entry)
	Touch(sessionID string) bool
	Delete(sessionID string)
	DeleteByWorker(workerID string) int
	CountForWorker(workerID string) int
	CountForWorkers(workerIDs []string) int
	RerouteWorker(oldWorkerID, newWorkerID string) int
	EntriesForWorker(workerID string) map[string]Entry
	SweepIdle(maxAge time.Duration) int
}
