package task

import "testing"

func TestCreateAndStatus(t *testing.T) {
	s := NewStore()
	s.Create("task-1", "")

	status, ok := s.Status("task-1")
	if !ok {
		t.Fatal("expected task to exist")
	}
	if status != Running {
		t.Errorf("expected Running, got %d", status)
	}
}

func TestStatusMissing(t *testing.T) {
	s := NewStore()
	_, ok := s.Status("nonexistent")
	if ok {
		t.Error("expected false for missing task")
	}
}

func TestSubscribeAndWrite(t *testing.T) {
	s := NewStore()
	sender := s.Create("task-1", "")

	sender.Write("line 1")
	sender.Write("line 2")

	snapshot, live, done, ok := s.Subscribe("task-1")
	if !ok {
		t.Fatal("expected task to exist")
	}
	if len(snapshot) != 2 {
		t.Errorf("expected 2 snapshot lines, got %d", len(snapshot))
	}
	if snapshot[0] != "line 1" || snapshot[1] != "line 2" {
		t.Errorf("unexpected snapshot: %v", snapshot)
	}

	// No dedup needed — live channel only delivers lines written
	// after Subscribe was called. Write a new line and verify it
	// arrives without any overlap.
	sender.Write("line 3")
	select {
	case line := <-live:
		if line != "line 3" {
			t.Errorf("expected 'line 3', got %q", line)
		}
	default:
		t.Error("expected line on live channel")
	}

	// done should not be closed yet
	select {
	case <-done:
		t.Error("done should not be closed yet")
	default:
	}
}

func TestSubscribeNoDuplicates(t *testing.T) {
	s := NewStore()
	sender := s.Create("task-1", "")

	// Write lines before subscribing
	sender.Write("before-1")
	sender.Write("before-2")

	snapshot, live, _, ok := s.Subscribe("task-1")
	if !ok {
		t.Fatal("expected task to exist")
	}
	if len(snapshot) != 2 {
		t.Fatalf("expected 2 snapshot lines, got %d", len(snapshot))
	}

	// The live channel should be empty — no overlap with snapshot
	select {
	case line := <-live:
		t.Errorf("expected empty live channel, got %q", line)
	default:
		// expected
	}

	// New line after subscribe should appear on live
	sender.Write("after-1")
	select {
	case line := <-live:
		if line != "after-1" {
			t.Errorf("expected 'after-1', got %q", line)
		}
	default:
		t.Error("expected line on live channel")
	}
}

func TestComplete(t *testing.T) {
	s := NewStore()
	sender := s.Create("task-1", "")

	sender.Write("line 1")
	sender.Complete(Completed)

	status, ok := s.Status("task-1")
	if !ok {
		t.Fatal("expected task to exist")
	}
	if status != Completed {
		t.Errorf("expected Completed, got %d", status)
	}

	snapshot, live, done, ok := s.Subscribe("task-1")
	if !ok {
		t.Fatal("expected task to exist")
	}
	if len(snapshot) != 1 {
		t.Errorf("expected 1 snapshot line, got %d", len(snapshot))
	}

	// live channel should be closed for completed tasks
	_, chanOpen := <-live
	if chanOpen {
		t.Error("expected live channel to be closed for completed task")
	}

	select {
	case <-done:
		// expected
	default:
		t.Error("done should be closed after Complete")
	}
}

func TestCreatedAt(t *testing.T) {
	s := NewStore()
	s.Create("task-1", "")

	ts := s.CreatedAt("task-1")
	if ts == "" {
		t.Fatal("expected non-empty timestamp")
	}

	// Non-existent task
	if got := s.CreatedAt("nonexistent"); got != "" {
		t.Errorf("expected empty string for missing task, got %q", got)
	}
}

func TestSubscribeMissing(t *testing.T) {
	s := NewStore()
	_, _, _, ok := s.Subscribe("nonexistent")
	if ok {
		t.Error("expected false for missing task")
	}
}

func TestCompleteFailed(t *testing.T) {
	s := NewStore()
	sender := s.Create("task-1", "")
	sender.Complete(Failed)

	status, _ := s.Status("task-1")
	if status != Failed {
		t.Errorf("expected Failed, got %d", status)
	}
}
