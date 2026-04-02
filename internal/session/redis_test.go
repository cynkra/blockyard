package session

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

func newRedisStore(t *testing.T) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	return NewRedisStore(client, time.Hour), mr
}

func TestRedisStoreGetSet(t *testing.T) {
	s, _ := newRedisStore(t)
	now := time.Now().Truncate(time.Second)
	s.Set("sess-1", Entry{WorkerID: "worker-1", UserSub: "user-a", LastAccess: now})

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
	if !e.LastAccess.Equal(now) {
		t.Errorf("expected %v, got %v", now, e.LastAccess)
	}
}

func TestRedisStoreGetMissing(t *testing.T) {
	s, _ := newRedisStore(t)
	_, ok := s.Get("nonexistent")
	if ok {
		t.Error("expected false for missing session")
	}
}

func TestRedisStoreDelete(t *testing.T) {
	s, _ := newRedisStore(t)
	s.Set("sess-1", Entry{WorkerID: "worker-1"})
	s.Delete("sess-1")

	_, ok := s.Get("sess-1")
	if ok {
		t.Error("expected session to be deleted")
	}
}

func TestRedisStoreDeleteByWorker(t *testing.T) {
	s, _ := newRedisStore(t)
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

func TestRedisStoreCountForWorker(t *testing.T) {
	s, _ := newRedisStore(t)
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

func TestRedisStoreCountForWorkers(t *testing.T) {
	s, _ := newRedisStore(t)
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

func TestRedisStoreRerouteWorker(t *testing.T) {
	s, _ := newRedisStore(t)
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

func TestRedisStoreEntriesForWorker(t *testing.T) {
	s, _ := newRedisStore(t)
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

func TestRedisStoreSweepIdleIsNoOp(t *testing.T) {
	s, _ := newRedisStore(t)
	s.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now().Add(-2 * time.Hour)})
	s.Set("sess-2", Entry{WorkerID: "w1", LastAccess: time.Now()})

	n := s.SweepIdle(time.Hour)
	if n != 0 {
		t.Errorf("SweepIdle should return 0, got %d", n)
	}

	// Both sessions should still exist (TTL handles expiry, not sweep).
	if _, ok := s.Get("sess-1"); !ok {
		t.Error("sess-1 should still exist")
	}
	if _, ok := s.Get("sess-2"); !ok {
		t.Error("sess-2 should still exist")
	}
}

func TestRedisStoreTouch(t *testing.T) {
	s, _ := newRedisStore(t)
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	s.Set("sess-1", Entry{WorkerID: "w1", LastAccess: old})

	if !s.Touch("sess-1") {
		t.Fatal("expected Touch to return true")
	}
	e, _ := s.Get("sess-1")
	if !e.LastAccess.After(old) {
		t.Error("expected LastAccess to be updated")
	}

	if s.Touch("nonexistent") {
		t.Error("expected Touch to return false for missing session")
	}
}

func TestRedisStoreTTLRefreshOnTouch(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	s := NewRedisStore(client, 10*time.Second)

	s.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})

	// Advance past half the TTL.
	mr.FastForward(6 * time.Second)

	// Touch should refresh TTL.
	if !s.Touch("sess-1") {
		t.Fatal("Touch should return true")
	}

	// Advance again past the original TTL but within the refreshed TTL.
	mr.FastForward(6 * time.Second)

	if _, ok := s.Get("sess-1"); !ok {
		t.Error("session should still exist after TTL refresh via Touch")
	}
}

func TestRedisStoreTTLExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	s := NewRedisStore(client, 10*time.Second)

	s.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})

	// Advance past TTL.
	mr.FastForward(11 * time.Second)

	if _, ok := s.Get("sess-1"); ok {
		t.Error("session should have expired")
	}
}
