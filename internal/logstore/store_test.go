package logstore

import (
	"testing"
	"time"
)

func TestCreateAndSubscribe(t *testing.T) {
	s := NewStore()
	sender := s.Create("worker-1", "app-1")

	sender.Write("log line 1")
	sender.Write("log line 2")

	snapshot, live, ok := s.Subscribe("worker-1")
	if !ok {
		t.Fatal("expected worker log to exist")
	}
	if len(snapshot) != 2 {
		t.Errorf("expected 2 snapshot lines, got %d", len(snapshot))
	}

	// Drain any buffered lines that overlap with the snapshot
	drained := 0
	for drained < len(snapshot) {
		select {
		case <-live:
			drained++
		default:
			drained = len(snapshot)
		}
	}

	// Write after subscribe
	sender.Write("log line 3")
	select {
	case line := <-live:
		if line != "log line 3" {
			t.Errorf("expected 'log line 3', got %q", line)
		}
	default:
		t.Error("expected line on live channel")
	}
}

func TestSubscribeMissing(t *testing.T) {
	s := NewStore()
	_, _, ok := s.Subscribe("nonexistent")
	if ok {
		t.Error("expected false for missing worker")
	}
}

func TestMarkEnded(t *testing.T) {
	s := NewStore()
	s.Create("worker-1", "app-1")

	if !s.HasActive("worker-1") {
		t.Error("expected active before MarkEnded")
	}

	s.MarkEnded("worker-1")

	if s.HasActive("worker-1") {
		t.Error("expected inactive after MarkEnded")
	}
}

func TestCleanupExpired(t *testing.T) {
	s := NewStore()
	s.Create("worker-1", "app-1")
	s.Create("worker-2", "app-1")

	s.MarkEnded("worker-1")

	// With zero retention, ended entries should be cleaned up
	n := s.CleanupExpired(0)
	if n != 1 {
		t.Errorf("expected 1 cleaned up, got %d", n)
	}

	// worker-2 is still active, should not be cleaned up
	if !s.HasActive("worker-2") {
		t.Error("worker-2 should still be active")
	}

	_, _, ok := s.Subscribe("worker-1")
	if ok {
		t.Error("worker-1 should have been cleaned up")
	}
}

func TestCleanupRespectsRetention(t *testing.T) {
	s := NewStore()
	s.Create("worker-1", "app-1")
	s.MarkEnded("worker-1")

	// With 1 hour retention, recently ended entry should NOT be cleaned up
	n := s.CleanupExpired(1 * time.Hour)
	if n != 0 {
		t.Errorf("expected 0 cleaned up with 1h retention, got %d", n)
	}
}

func TestWorkerIDsByApp(t *testing.T) {
	s := NewStore()
	s.Create("worker-1", "app-1")
	s.Create("worker-2", "app-1")
	s.Create("worker-3", "app-2")

	ids := s.WorkerIDsByApp("app-1")
	if len(ids) != 2 {
		t.Errorf("expected 2 worker IDs for app-1, got %d", len(ids))
	}
}

func TestMarkEndedIdempotent(t *testing.T) {
	s := NewStore()
	s.Create("worker-1", "app-1")

	s.MarkEnded("worker-1")
	s.MarkEnded("worker-1") // second call must not panic
}

func TestMarkEndedNonexistent(t *testing.T) {
	s := NewStore()
	s.MarkEnded("nonexistent") // must not panic
}

func TestIsEnded(t *testing.T) {
	s := NewStore()
	s.Create("worker-1", "app-1")

	if s.IsEnded("worker-1") {
		t.Error("expected not ended before MarkEnded")
	}

	s.MarkEnded("worker-1")

	if !s.IsEnded("worker-1") {
		t.Error("expected ended after MarkEnded")
	}

	// Nonexistent worker
	if s.IsEnded("nonexistent") {
		t.Error("expected false for nonexistent worker")
	}
}

func TestBufferCap(t *testing.T) {
	s := NewStore()
	sender := s.Create("worker-1", "app-1")

	for i := 0; i < maxLogLines+100; i++ {
		sender.Write("line")
	}

	snapshot, _, ok := s.Subscribe("worker-1")
	if !ok {
		t.Fatal("expected worker log to exist")
	}
	if len(snapshot) != maxLogLines {
		t.Errorf("expected buffer capped at %d, got %d", maxLogLines, len(snapshot))
	}
}
