package proxy

import (
	"testing"

	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
)

func TestAssignEmptyWorkers(t *testing.T) {
	lb := LoadBalancer{}
	workers := server.NewWorkerMap()
	sessions := session.NewStore()

	wid, err := lb.Assign("app-1", workers, sessions, 5, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wid != "" {
		t.Errorf("expected empty worker ID (spawn signal), got %q", wid)
	}
}

func TestAssignSingleWorkerWithCapacity(t *testing.T) {
	lb := LoadBalancer{}
	workers := server.NewWorkerMap()
	sessions := session.NewStore()

	workers.Set("w1", server.ActiveWorker{AppID: "app-1"})
	sessions.Set("s1", session.Entry{WorkerID: "w1"})

	wid, err := lb.Assign("app-1", workers, sessions, 5, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wid != "w1" {
		t.Errorf("expected w1, got %q", wid)
	}
}

func TestAssignLeastLoaded(t *testing.T) {
	lb := LoadBalancer{}
	workers := server.NewWorkerMap()
	sessions := session.NewStore()

	workers.Set("w1", server.ActiveWorker{AppID: "app-1"})
	workers.Set("w2", server.ActiveWorker{AppID: "app-1"})

	// w1 has 3 sessions, w2 has 1 session
	sessions.Set("s1", session.Entry{WorkerID: "w1"})
	sessions.Set("s2", session.Entry{WorkerID: "w1"})
	sessions.Set("s3", session.Entry{WorkerID: "w1"})
	sessions.Set("s4", session.Entry{WorkerID: "w2"})

	wid, err := lb.Assign("app-1", workers, sessions, 5, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wid != "w2" {
		t.Errorf("expected w2 (least loaded), got %q", wid)
	}
}

func TestAssignNoCapacityCanScale(t *testing.T) {
	lb := LoadBalancer{}
	workers := server.NewWorkerMap()
	sessions := session.NewStore()

	workers.Set("w1", server.ActiveWorker{AppID: "app-1"})
	sessions.Set("s1", session.Entry{WorkerID: "w1"})
	sessions.Set("s2", session.Entry{WorkerID: "w1"})

	// max_sessions_per_worker = 2, worker is full, max_workers_per_app = nil (unlimited)
	wid, err := lb.Assign("app-1", workers, sessions, 2, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wid != "" {
		t.Errorf("expected empty worker ID (spawn signal), got %q", wid)
	}
}

func TestAssignNoCapacityCanScaleWithLimit(t *testing.T) {
	lb := LoadBalancer{}
	workers := server.NewWorkerMap()
	sessions := session.NewStore()

	workers.Set("w1", server.ActiveWorker{AppID: "app-1"})
	sessions.Set("s1", session.Entry{WorkerID: "w1"})

	maxWorkers := 3
	// max_sessions = 1 (full), but max_workers = 3 (can scale)
	wid, err := lb.Assign("app-1", workers, sessions, 1, &maxWorkers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wid != "" {
		t.Errorf("expected empty worker ID (spawn signal), got %q", wid)
	}
}

func TestAssignCapacityExhausted(t *testing.T) {
	lb := LoadBalancer{}
	workers := server.NewWorkerMap()
	sessions := session.NewStore()

	workers.Set("w1", server.ActiveWorker{AppID: "app-1"})
	workers.Set("w2", server.ActiveWorker{AppID: "app-1"})
	sessions.Set("s1", session.Entry{WorkerID: "w1"})
	sessions.Set("s2", session.Entry{WorkerID: "w2"})

	maxWorkers := 2
	// Both workers full (max_sessions = 1), at max_workers limit
	wid, err := lb.Assign("app-1", workers, sessions, 1, &maxWorkers)
	if err != errCapacityExhausted {
		t.Fatalf("expected errCapacityExhausted, got %v", err)
	}
	if wid != "" {
		t.Errorf("expected empty worker ID, got %q", wid)
	}
}

func TestAssignIgnoresOtherApps(t *testing.T) {
	lb := LoadBalancer{}
	workers := server.NewWorkerMap()
	sessions := session.NewStore()

	workers.Set("w1", server.ActiveWorker{AppID: "app-1"})
	workers.Set("w2", server.ActiveWorker{AppID: "app-2"})

	// app-2's worker should not be considered for app-1
	wid, err := lb.Assign("app-1", workers, sessions, 5, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wid != "w1" {
		t.Errorf("expected w1, got %q", wid)
	}
}

func TestAssignWorkerAtExactCapacity(t *testing.T) {
	lb := LoadBalancer{}
	workers := server.NewWorkerMap()
	sessions := session.NewStore()

	workers.Set("w1", server.ActiveWorker{AppID: "app-1"})
	sessions.Set("s1", session.Entry{WorkerID: "w1"})
	sessions.Set("s2", session.Entry{WorkerID: "w1"})
	sessions.Set("s3", session.Entry{WorkerID: "w1"})

	// Worker has exactly max sessions
	maxWorkers := 1
	wid, err := lb.Assign("app-1", workers, sessions, 3, &maxWorkers)
	if err != errCapacityExhausted {
		t.Fatalf("expected errCapacityExhausted, got err=%v, wid=%q", err, wid)
	}
}
