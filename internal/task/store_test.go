package task

import "testing"

func TestCreateAndStatus(t *testing.T) {
	s := NewStore()
	s.Create("task-1")

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
	sender := s.Create("task-1")

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

	// Drain any buffered lines that overlap with the snapshot
	drained := 0
	for drained < len(snapshot) {
		select {
		case <-live:
			drained++
		default:
			// Channel may not have all overlap lines buffered
			drained = len(snapshot)
		}
	}

	// Write after subscribe — should appear on live channel
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

func TestComplete(t *testing.T) {
	s := NewStore()
	sender := s.Create("task-1")

	sender.Write("line 1")
	sender.Complete(Completed)

	status, ok := s.Status("task-1")
	if !ok {
		t.Fatal("expected task to exist")
	}
	if status != Completed {
		t.Errorf("expected Completed, got %d", status)
	}

	_, _, done, ok := s.Subscribe("task-1")
	if !ok {
		t.Fatal("expected task to exist")
	}
	select {
	case <-done:
		// expected
	default:
		t.Error("done should be closed after Complete")
	}
}

func TestCompleteFailed(t *testing.T) {
	s := NewStore()
	sender := s.Create("task-1")
	sender.Complete(Failed)

	status, _ := s.Status("task-1")
	if status != Failed {
		t.Errorf("expected Failed, got %d", status)
	}
}
