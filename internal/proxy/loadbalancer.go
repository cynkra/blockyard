package proxy

import (
	"errors"

	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
)

var errCapacityExhausted = errors.New("all workers at capacity")

// LoadBalancer assigns new sessions to workers using a least-loaded
// strategy. Stateless — decisions are based on current worker and
// session state at call time.
type LoadBalancer struct{}

// Assign picks a worker for a new session belonging to appID.
//
// Returns a worker ID when an existing worker has available capacity.
// Returns ("", nil) when no worker has capacity but the per-app limit
// has not been reached — the caller should spawn a new worker.
// Returns errCapacityExhausted when all workers are full and the
// per-app limit has been reached.
func (lb *LoadBalancer) Assign(
	appID string,
	workers *server.WorkerMap,
	sessions *session.Store,
	maxSessionsPerWorker int,
	maxWorkersPerApp *int,
) (string, error) {
	workerIDs := workers.ForAppAvailable(appID)
	if len(workerIDs) == 0 {
		return "", nil // no workers yet — caller spawns
	}

	// Find least-loaded worker with available capacity.
	bestID := ""
	bestCount := maxSessionsPerWorker // upper bound

	for _, wid := range workerIDs {
		count := sessions.CountForWorker(wid)
		if count < maxSessionsPerWorker && count < bestCount {
			bestID = wid
			bestCount = count
		}
	}

	if bestID != "" {
		return bestID, nil
	}

	// All workers at capacity — can we spawn more?
	if maxWorkersPerApp == nil || len(workerIDs) < *maxWorkersPerApp {
		return "", nil // caller spawns
	}

	return "", errCapacityExhausted
}
