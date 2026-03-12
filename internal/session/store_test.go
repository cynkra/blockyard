package session

import "testing"

func TestSetAndGet(t *testing.T) {
	s := NewStore()
	s.Set("sess-1", "worker-1")

	wid, ok := s.Get("sess-1")
	if !ok {
		t.Fatal("expected session to exist")
	}
	if wid != "worker-1" {
		t.Errorf("expected worker-1, got %q", wid)
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
	s.Set("sess-1", "worker-1")
	s.Delete("sess-1")

	_, ok := s.Get("sess-1")
	if ok {
		t.Error("expected session to be deleted")
	}
}

func TestDeleteByWorker(t *testing.T) {
	s := NewStore()
	s.Set("sess-1", "worker-1")
	s.Set("sess-2", "worker-1")
	s.Set("sess-3", "worker-2")

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
	s.Set("sess-1", "worker-1")
	s.Set("sess-2", "worker-1")
	s.Set("sess-3", "worker-2")

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
