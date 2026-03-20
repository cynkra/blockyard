package server

import (
	"testing"
	"time"
)

func TestWorkerMapCountForApp(t *testing.T) {
	m := NewWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})
	m.Set("w2", ActiveWorker{AppID: "app-a"})
	m.Set("w3", ActiveWorker{AppID: "app-b"})

	if got := m.CountForApp("app-a"); got != 2 {
		t.Errorf("expected 2 for app-a, got %d", got)
	}
	if got := m.CountForApp("app-b"); got != 1 {
		t.Errorf("expected 1 for app-b, got %d", got)
	}
	if got := m.CountForApp("app-c"); got != 0 {
		t.Errorf("expected 0 for app-c, got %d", got)
	}
}

func TestWorkerMapForApp(t *testing.T) {
	m := NewWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})
	m.Set("w2", ActiveWorker{AppID: "app-b"})
	m.Set("w3", ActiveWorker{AppID: "app-a"})

	ids := m.ForApp("app-a")
	if len(ids) != 2 {
		t.Fatalf("expected 2 workers for app-a, got %d", len(ids))
	}

	ids = m.ForApp("app-c")
	if len(ids) != 0 {
		t.Fatalf("expected 0 workers for app-c, got %d", len(ids))
	}
}

func TestWorkerMapCRUD(t *testing.T) {
	m := NewWorkerMap()

	if m.Count() != 0 {
		t.Fatalf("expected empty map, got %d", m.Count())
	}

	m.Set("w1", ActiveWorker{AppID: "app-a"})
	w, ok := m.Get("w1")
	if !ok || w.AppID != "app-a" {
		t.Fatal("expected to get worker w1")
	}

	_, ok = m.Get("nonexistent")
	if ok {
		t.Fatal("expected false for nonexistent worker")
	}

	m.Delete("w1")
	if m.Count() != 0 {
		t.Fatalf("expected 0 after delete, got %d", m.Count())
	}
}

func TestWorkerMapAll(t *testing.T) {
	m := NewWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})
	m.Set("w2", ActiveWorker{AppID: "app-b"})

	all := m.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 workers, got %d", len(all))
	}
}

func TestIdleWorkersScaleToZero(t *testing.T) {
	m := NewWorkerMap()
	// Single worker for app, idle beyond timeout — should be returned.
	m.Set("w1", ActiveWorker{
		AppID:     "app-a",
		IdleSince: time.Now().Add(-10 * time.Minute),
	})

	idle := m.IdleWorkers(5 * time.Minute)
	if len(idle) != 1 {
		t.Errorf("expected 1 idle worker (scale to zero), got %d", len(idle))
	}
}

func TestIdleWorkersExcludesDraining(t *testing.T) {
	m := NewWorkerMap()
	m.Set("w1", ActiveWorker{
		AppID:     "app-a",
		Draining:  true,
		IdleSince: time.Now().Add(-10 * time.Minute),
	})

	idle := m.IdleWorkers(5 * time.Minute)
	if len(idle) != 0 {
		t.Errorf("expected 0 idle workers (draining excluded), got %d", len(idle))
	}
}

func TestIdleWorkersExcludesNotYetIdle(t *testing.T) {
	m := NewWorkerMap()
	m.Set("w1", ActiveWorker{
		AppID:     "app-a",
		IdleSince: time.Now().Add(-1 * time.Minute),
	})

	idle := m.IdleWorkers(5 * time.Minute)
	if len(idle) != 0 {
		t.Errorf("expected 0 idle workers (not yet idle enough), got %d", len(idle))
	}
}

func TestClearIdleSinceReturnsBool(t *testing.T) {
	m := NewWorkerMap()
	m.Set("w1", ActiveWorker{
		AppID:     "app-a",
		IdleSince: time.Now().Add(-5 * time.Minute),
	})
	m.Set("w2", ActiveWorker{AppID: "app-b"}) // not idle

	// Clearing an idle worker returns true.
	if wasIdle := m.ClearIdleSince("w1"); !wasIdle {
		t.Error("expected ClearIdleSince to return true for idle worker")
	}

	// Clearing a non-idle worker returns false.
	if wasIdle := m.ClearIdleSince("w2"); wasIdle {
		t.Error("expected ClearIdleSince to return false for non-idle worker")
	}

	// Clearing nonexistent worker returns false.
	if wasIdle := m.ClearIdleSince("nonexistent"); wasIdle {
		t.Error("expected ClearIdleSince to return false for nonexistent worker")
	}

	// After clearing, the worker is no longer idle.
	if wasIdle := m.ClearIdleSince("w1"); wasIdle {
		t.Error("expected ClearIdleSince to return false after already cleared")
	}
}
