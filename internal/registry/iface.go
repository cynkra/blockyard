package registry

// WorkerRegistry defines the contract for worker address lookup.
// MemoryRegistry is the in-process implementation; Redis implements
// the same interface for shared state during rolling updates.
type WorkerRegistry interface {
	Get(workerID string) (string, bool)
	Set(workerID string, addr string)
	Delete(workerID string)
}
