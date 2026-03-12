package server

import "testing"

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
