package session

import (
	"testing"
	"time"
)

func TestSetAndGet(t *testing.T) {
	s := NewStore()
	s.Set("sess-1", Entry{WorkerID: "worker-1", UserSub: "user-a", LastAccess: time.Now()})

	e, ok := s.Get("sess-1")
	if !ok {
		t.Fatal("expected session to exist")
	}
	if e.WorkerID != "worker-1" {
		t.Errorf("expected worker-1, got %q", e.WorkerID)
	}
	if e.UserSub != "user-a" {
		t.Errorf("expected user-a, got %q", e.UserSub)
	}
}

func TestGetMissing(t *testing.T) {
	s := NewStore()
	_, ok := s.Get("nonexistent")
	if ok {
		t.Error("expected false for missing session")
	}
}

func TestDelete(t *testing.T) {
	s := NewStore()
	s.Set("sess-1", Entry{WorkerID: "worker-1"})
	s.Delete("sess-1")

	_, ok := s.Get("sess-1")
	if ok {
		t.Error("expected session to be deleted")
	}
}

func TestDeleteByWorker(t *testing.T) {
	s := NewStore()
	s.Set("sess-1", Entry{WorkerID: "worker-1"})
	s.Set("sess-2", Entry{WorkerID: "worker-1"})
	s.Set("sess-3", Entry{WorkerID: "worker-2"})

	n := s.DeleteByWorker("worker-1")
	if n != 2 {
		t.Errorf("expected 2 deleted, got %d", n)
	}

	_, ok := s.Get("sess-1")
	if ok {
		t.Error("sess-1 should be deleted")
	}
	_, ok = s.Get("sess-3")
	if !ok {
		t.Error("sess-3 should still exist")
	}
}

func TestCountForWorker(t *testing.T) {
	s := NewStore()
	s.Set("sess-1", Entry{WorkerID: "worker-1"})
	s.Set("sess-2", Entry{WorkerID: "worker-1"})
	s.Set("sess-3", Entry{WorkerID: "worker-2"})

	if n := s.CountForWorker("worker-1"); n != 2 {
		t.Errorf("expected 2, got %d", n)
	}
	if n := s.CountForWorker("worker-2"); n != 1 {
		t.Errorf("expected 1, got %d", n)
	}
	if n := s.CountForWorker("worker-3"); n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestTouch(t *testing.T) {
	s := NewStore()
	old := time.Now().Add(-time.Hour)
	s.Set("sess-1", Entry{WorkerID: "w1", LastAccess: old})

	if !s.Touch("sess-1") {
		t.Fatal("expected Touch to return true")
	}
	e, _ := s.Get("sess-1")
	if e.LastAccess.Before(old.Add(time.Minute)) {
		t.Error("expected LastAccess to be updated")
	}

	if s.Touch("nonexistent") {
		t.Error("expected Touch to return false for missing session")
	}
}

func TestRerouteWorker(t *testing.T) {
	s := NewStore()
	s.Set("s1", Entry{WorkerID: "old-worker", LastAccess: time.Now()})
	s.Set("s2", Entry{WorkerID: "old-worker", LastAccess: time.Now()})
	s.Set("s3", Entry{WorkerID: "other-worker", LastAccess: time.Now()})

	n := s.RerouteWorker("old-worker", "new-worker")
	if n != 2 {
		t.Errorf("rerouted %d sessions, want 2", n)
	}

	e1, _ := s.Get("s1")
	if e1.WorkerID != "new-worker" {
		t.Errorf("s1 worker = %q, want %q", e1.WorkerID, "new-worker")
	}
	e3, _ := s.Get("s3")
	if e3.WorkerID != "other-worker" {
		t.Errorf("s3 should be unchanged, got %q", e3.WorkerID)
	}
}

func TestCountForWorkers(t *testing.T) {
	s := NewStore()
	s.Set("sess-1", Entry{WorkerID: "worker-1"})
	s.Set("sess-2", Entry{WorkerID: "worker-1"})
	s.Set("sess-3", Entry{WorkerID: "worker-2"})
	s.Set("sess-4", Entry{WorkerID: "worker-3"})

	if n := s.CountForWorkers([]string{"worker-1", "worker-2"}); n != 3 {
		t.Errorf("expected 3, got %d", n)
	}
	if n := s.CountForWorkers([]string{"worker-3"}); n != 1 {
		t.Errorf("expected 1, got %d", n)
	}
	if n := s.CountForWorkers(nil); n != 0 {
		t.Errorf("expected 0 for nil, got %d", n)
	}
	if n := s.CountForWorkers([]string{"nonexistent"}); n != 0 {
		t.Errorf("expected 0 for nonexistent, got %d", n)
	}
}

func TestEntriesForWorker(t *testing.T) {
	s := NewStore()
	s.Set("s1", Entry{WorkerID: "w1", UserSub: "user-a", LastAccess: time.Now()})
	s.Set("s2", Entry{WorkerID: "w1", UserSub: "user-b", LastAccess: time.Now()})
	s.Set("s3", Entry{WorkerID: "w2", UserSub: "user-c", LastAccess: time.Now()})

	entries := s.EntriesForWorker("w1")
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries for w1, got %d", len(entries))
	}
	if _, ok := entries["s1"]; !ok {
		t.Error("expected s1 in entries")
	}
	if _, ok := entries["s2"]; !ok {
		t.Error("expected s2 in entries")
	}

	empty := s.EntriesForWorker("nonexistent")
	if len(empty) != 0 {
		t.Errorf("expected 0 entries for nonexistent, got %d", len(empty))
	}
}

func TestSweepIdle(t *testing.T) {
	s := NewStore()
	s.Set("old", Entry{WorkerID: "w1", LastAccess: time.Now().Add(-2 * time.Hour)})
	s.Set("recent", Entry{WorkerID: "w1", LastAccess: time.Now()})

	n := s.SweepIdle(time.Hour)
	if n != 1 {
		t.Errorf("expected 1 swept, got %d", n)
	}

	if _, ok := s.Get("old"); ok {
		t.Error("old session should have been swept")
	}
	if _, ok := s.Get("recent"); !ok {
		t.Error("recent session should still exist")
	}
}
