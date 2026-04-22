package session

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// storeFactory returns a fresh Store for each test.
type storeFactory func(t *testing.T) Store

func storeImplementations(t *testing.T) map[string]storeFactory {
	t.Helper()
	return map[string]storeFactory{
		"Memory": func(t *testing.T) Store {
			t.Helper()
			return NewMemoryStore()
		},
		"Redis": func(t *testing.T) Store {
			t.Helper()
			mr := miniredis.RunT(t)
			client := redisstate.TestClient(t, mr.Addr())
			return NewRedisStore(client, time.Hour)
		},
		"Postgres": func(t *testing.T) Store {
			t.Helper()
			return NewPostgresStore(testPGDB(t), time.Hour)
		},
		"Layered": func(t *testing.T) Store {
			t.Helper()
			// Matches production wiring: Postgres primary + Redis cache.
			mr := miniredis.RunT(t)
			client := redisstate.TestClient(t, mr.Addr())
			cache := NewRedisStore(client, time.Hour)
			primary := NewPostgresStore(testPGDB(t), time.Hour)
			return NewLayeredStore(primary, cache)
		},
	}
}

func TestStoreConformance_EmptyState(t *testing.T) {
	for name, factory := range storeImplementations(t) {
		t.Run(name, func(t *testing.T) {
			s := factory(t)

			if _, ok := s.Get("nonexistent"); ok {
				t.Error("Get(nonexistent) should return false")
			}
			if s.Touch("nonexistent") {
				t.Error("Touch(nonexistent) should return false")
			}
			if n := s.CountForWorker("w1"); n != 0 {
				t.Errorf("CountForWorker(w1) = %d, want 0", n)
			}
			if n := s.CountForWorkers([]string{"w1", "w2"}); n != 0 {
				t.Errorf("CountForWorkers = %d, want 0", n)
			}
			if n := s.CountForWorkers(nil); n != 0 {
				t.Errorf("CountForWorkers(nil) = %d, want 0", n)
			}
			if n := s.DeleteByWorker("w1"); n != 0 {
				t.Errorf("DeleteByWorker(w1) = %d, want 0", n)
			}
			if n := s.RerouteWorker("old", "new"); n != 0 {
				t.Errorf("RerouteWorker = %d, want 0", n)
			}
			entries := s.EntriesForWorker("w1")
			if len(entries) != 0 {
				t.Errorf("EntriesForWorker(w1) = %v, want empty", entries)
			}
		})
	}
}

func TestStoreConformance_SetOverwrite(t *testing.T) {
	for name, factory := range storeImplementations(t) {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			now := time.Now().Truncate(time.Second)

			s.Set("sess-1", Entry{WorkerID: "w1", UserSub: "user-a", LastAccess: now})
			s.Set("sess-1", Entry{WorkerID: "w2", UserSub: "user-b", LastAccess: now})

			e, ok := s.Get("sess-1")
			if !ok {
				t.Fatal("expected session to exist")
			}
			if e.WorkerID != "w2" {
				t.Errorf("WorkerID = %q, want %q", e.WorkerID, "w2")
			}
			if e.UserSub != "user-b" {
				t.Errorf("UserSub = %q, want %q", e.UserSub, "user-b")
			}

			// Overwrite should not create duplicates.
			if n := s.CountForWorker("w2"); n != 1 {
				t.Errorf("CountForWorker(w2) = %d, want 1", n)
			}
			if n := s.CountForWorker("w1"); n != 0 {
				t.Errorf("CountForWorker(w1) = %d, want 0 after overwrite", n)
			}
		})
	}
}

func TestStoreConformance_DeleteNonexistent(t *testing.T) {
	for name, factory := range storeImplementations(t) {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			// Should not panic.
			s.Delete("nonexistent")
		})
	}
}

func TestStoreConformance_DeleteByWorkerNoMatch(t *testing.T) {
	for name, factory := range storeImplementations(t) {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			s.Set("sess-1", Entry{WorkerID: "w1"})

			n := s.DeleteByWorker("w2")
			if n != 0 {
				t.Errorf("DeleteByWorker(w2) = %d, want 0", n)
			}
			// Original session should be untouched.
			if _, ok := s.Get("sess-1"); !ok {
				t.Error("sess-1 should still exist")
			}
		})
	}
}

func TestStoreConformance_RerouteWorkerNoMatch(t *testing.T) {
	for name, factory := range storeImplementations(t) {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			s.Set("sess-1", Entry{WorkerID: "w1", UserSub: "user-a"})

			n := s.RerouteWorker("nonexistent", "w2")
			if n != 0 {
				t.Errorf("RerouteWorker(nonexistent) = %d, want 0", n)
			}
			// Original session should be untouched.
			e, _ := s.Get("sess-1")
			if e.WorkerID != "w1" {
				t.Errorf("WorkerID = %q, want %q", e.WorkerID, "w1")
			}
		})
	}
}

func TestStoreConformance_RerouteWorkerPreservesFields(t *testing.T) {
	for name, factory := range storeImplementations(t) {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			now := time.Now().Truncate(time.Second)

			s.Set("sess-1", Entry{WorkerID: "old-w", UserSub: "user-a", LastAccess: now})
			s.Set("sess-2", Entry{WorkerID: "old-w", UserSub: "user-b", LastAccess: now})

			n := s.RerouteWorker("old-w", "new-w")
			if n != 2 {
				t.Fatalf("RerouteWorker = %d, want 2", n)
			}

			e1, _ := s.Get("sess-1")
			if e1.WorkerID != "new-w" {
				t.Errorf("sess-1 WorkerID = %q, want %q", e1.WorkerID, "new-w")
			}
			if e1.UserSub != "user-a" {
				t.Errorf("sess-1 UserSub = %q, want %q (should be preserved)", e1.UserSub, "user-a")
			}
			if !e1.LastAccess.Equal(now) {
				t.Errorf("sess-1 LastAccess = %v, want %v (should be preserved)", e1.LastAccess, now)
			}

			e2, _ := s.Get("sess-2")
			if e2.UserSub != "user-b" {
				t.Errorf("sess-2 UserSub = %q, want %q (should be preserved)", e2.UserSub, "user-b")
			}
		})
	}
}

func TestStoreConformance_EntriesForWorkerFieldFidelity(t *testing.T) {
	for name, factory := range storeImplementations(t) {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			now := time.Now().Truncate(time.Second)

			s.Set("sess-1", Entry{WorkerID: "w1", UserSub: "user-a", LastAccess: now})
			s.Set("sess-2", Entry{WorkerID: "w1", UserSub: "user-b", LastAccess: now.Add(-time.Hour)})
			s.Set("sess-3", Entry{WorkerID: "w2", UserSub: "user-c", LastAccess: now})

			entries := s.EntriesForWorker("w1")
			if len(entries) != 2 {
				t.Fatalf("EntriesForWorker(w1) returned %d entries, want 2", len(entries))
			}

			e1, ok := entries["sess-1"]
			if !ok {
				t.Fatal("expected sess-1 in entries")
			}
			if e1.WorkerID != "w1" {
				t.Errorf("sess-1 WorkerID = %q, want %q", e1.WorkerID, "w1")
			}
			if e1.UserSub != "user-a" {
				t.Errorf("sess-1 UserSub = %q, want %q", e1.UserSub, "user-a")
			}
			if !e1.LastAccess.Equal(now) {
				t.Errorf("sess-1 LastAccess = %v, want %v", e1.LastAccess, now)
			}

			e2, ok := entries["sess-2"]
			if !ok {
				t.Fatal("expected sess-2 in entries")
			}
			if e2.UserSub != "user-b" {
				t.Errorf("sess-2 UserSub = %q, want %q", e2.UserSub, "user-b")
			}
			if !e2.LastAccess.Equal(now.Add(-time.Hour)) {
				t.Errorf("sess-2 LastAccess = %v, want %v", e2.LastAccess, now.Add(-time.Hour))
			}
		})
	}
}

func TestStoreConformance_TouchUpdatesLastAccess(t *testing.T) {
	for name, factory := range storeImplementations(t) {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			old := time.Now().Add(-time.Hour).Truncate(time.Second)

			s.Set("sess-1", Entry{WorkerID: "w1", UserSub: "user-a", LastAccess: old})

			if !s.Touch("sess-1") {
				t.Fatal("Touch should return true")
			}

			e, _ := s.Get("sess-1")
			if !e.LastAccess.After(old) {
				t.Error("LastAccess should be updated after Touch")
			}
			// Touch should not affect other fields.
			if e.WorkerID != "w1" {
				t.Errorf("WorkerID = %q, want %q (should be preserved)", e.WorkerID, "w1")
			}
			if e.UserSub != "user-a" {
				t.Errorf("UserSub = %q, want %q (should be preserved)", e.UserSub, "user-a")
			}
		})
	}
}

func TestStoreConformance_CountForWorkersSubset(t *testing.T) {
	for name, factory := range storeImplementations(t) {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			s.Set("s1", Entry{WorkerID: "w1"})
			s.Set("s2", Entry{WorkerID: "w1"})
			s.Set("s3", Entry{WorkerID: "w2"})
			s.Set("s4", Entry{WorkerID: "w3"})

			if n := s.CountForWorkers([]string{"w1", "w2"}); n != 3 {
				t.Errorf("CountForWorkers([w1,w2]) = %d, want 3", n)
			}
			if n := s.CountForWorkers([]string{"w3"}); n != 1 {
				t.Errorf("CountForWorkers([w3]) = %d, want 1", n)
			}
			if n := s.CountForWorkers([]string{"nonexistent"}); n != 0 {
				t.Errorf("CountForWorkers([nonexistent]) = %d, want 0", n)
			}
		})
	}
}

func TestStoreConformance_DeleteByWorkerSelectivity(t *testing.T) {
	for name, factory := range storeImplementations(t) {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			s.Set("s1", Entry{WorkerID: "w1"})
			s.Set("s2", Entry{WorkerID: "w1"})
			s.Set("s3", Entry{WorkerID: "w2"})

			n := s.DeleteByWorker("w1")
			if n != 2 {
				t.Errorf("DeleteByWorker(w1) = %d, want 2", n)
			}

			if _, ok := s.Get("s1"); ok {
				t.Error("s1 should be deleted")
			}
			if _, ok := s.Get("s2"); ok {
				t.Error("s2 should be deleted")
			}
			if _, ok := s.Get("s3"); !ok {
				t.Error("s3 should still exist")
			}
		})
	}
}
