package registry

import "testing"

func TestSetAndGet(t *testing.T) {
	r := NewMemoryRegistry()
	r.Set("worker-1", "127.0.0.1:3838")

	addr, ok := r.Get("worker-1")
	if !ok {
		t.Fatal("expected worker to exist")
	}
	if addr != "127.0.0.1:3838" {
		t.Errorf("expected 127.0.0.1:3838, got %q", addr)
	}
}

func TestGetMissing(t *testing.T) {
	r := NewMemoryRegistry()
	_, ok := r.Get("nonexistent")
	if ok {
		t.Error("expected false for missing worker")
	}
}

func TestDelete(t *testing.T) {
	r := NewMemoryRegistry()
	r.Set("worker-1", "127.0.0.1:3838")
	r.Delete("worker-1")

	_, ok := r.Get("worker-1")
	if ok {
		t.Error("expected worker to be deleted")
	}
}
