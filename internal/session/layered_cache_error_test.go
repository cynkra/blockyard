package session

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// Cache-failure behavior for LayeredStore (see #262). The whole point of
// making Postgres the primary is that Redis can go away — be restarted
// for cert rotation, evicted, partitioned — without user-visible impact.
// These tests pair a real PostgresStore primary with a real RedisStore
// cache whose miniredis is poisoned via SetError, then assert every
// Store method still returns the correct primary answer. Using the
// production primary (not MemoryStore) ensures the SQL path behaves
// correctly when the cache fails mid-operation.
//
// Skips when BLOCKYARD_TEST_POSTGRES_URL is not set. CI's `unit` job
// always provides one.

func newLayeredWithErroringCache(t *testing.T) (*LayeredStore, *PostgresStore, *miniredis.Miniredis) {
	t.Helper()
	primary := NewPostgresStore(testPGDB(t), time.Hour)
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	cache := NewRedisStore(client, time.Hour)
	return NewLayeredStore(primary, cache), primary, mr
}

func TestLayeredStore_CacheErrors_GetFallsBackToPrimary(t *testing.T) {
	s, primary, mr := newLayeredWithErroringCache(t)

	now := time.Now().Truncate(time.Second)
	primary.Set("sess-1", Entry{WorkerID: "w1", UserSub: "u", LastAccess: now})

	mr.SetError("READONLY simulated failure")

	e, ok := s.Get("sess-1")
	if !ok {
		t.Fatal("Get must succeed via primary when cache errors")
	}
	if e.WorkerID != "w1" {
		t.Errorf("WorkerID = %q, want %q", e.WorkerID, "w1")
	}
}

func TestLayeredStore_CacheErrors_SetPersistsPrimary(t *testing.T) {
	s, primary, mr := newLayeredWithErroringCache(t)
	mr.SetError("READONLY simulated failure")

	s.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})

	if _, ok := primary.Get("sess-1"); !ok {
		t.Error("primary must hold the entry even if cache write errored")
	}
}

func TestLayeredStore_CacheErrors_TouchReturnsPrimary(t *testing.T) {
	s, primary, mr := newLayeredWithErroringCache(t)
	primary.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now().Add(-time.Hour)})

	mr.SetError("READONLY simulated failure")

	if !s.Touch("sess-1") {
		t.Error("Touch must report true based on primary, not cache")
	}
}

func TestLayeredStore_CacheErrors_DeleteClearsPrimary(t *testing.T) {
	s, primary, mr := newLayeredWithErroringCache(t)
	primary.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})

	mr.SetError("READONLY simulated failure")

	s.Delete("sess-1")
	if _, ok := primary.Get("sess-1"); ok {
		t.Error("primary must be cleared even if cache delete errored")
	}
}

func TestLayeredStore_CacheErrors_DeleteByWorkerReturnsPrimaryCount(t *testing.T) {
	s, primary, mr := newLayeredWithErroringCache(t)
	primary.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})
	primary.Set("sess-2", Entry{WorkerID: "w1", LastAccess: time.Now()})

	mr.SetError("READONLY simulated failure")

	if n := s.DeleteByWorker("w1"); n != 2 {
		t.Errorf("DeleteByWorker = %d, want 2 (primary count)", n)
	}
}

func TestLayeredStore_CacheErrors_CountAggregatesFromPrimary(t *testing.T) {
	s, primary, mr := newLayeredWithErroringCache(t)
	primary.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})
	primary.Set("sess-2", Entry{WorkerID: "w1", LastAccess: time.Now()})

	mr.SetError("READONLY simulated failure")

	if n := s.CountForWorker("w1"); n != 2 {
		t.Errorf("CountForWorker = %d, want 2", n)
	}
	if n := s.CountForWorkers([]string{"w1"}); n != 2 {
		t.Errorf("CountForWorkers = %d, want 2", n)
	}
}

func TestLayeredStore_CacheErrors_RerouteRunsAgainstPrimary(t *testing.T) {
	s, primary, mr := newLayeredWithErroringCache(t)
	primary.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})

	mr.SetError("READONLY simulated failure")

	if n := s.RerouteWorker("w1", "w2"); n != 1 {
		t.Errorf("RerouteWorker = %d, want 1", n)
	}
	e, _ := primary.Get("sess-1")
	if e.WorkerID != "w2" {
		t.Errorf("primary worker_id = %q, want %q", e.WorkerID, "w2")
	}
}

// TestLayeredStore_CacheRestartRewarms simulates the parent DoD: Redis
// "restarts" (miniredis FlushAll wipes the dataset), and subsequent
// reads repopulate the cache from the Postgres primary without
// user-visible impact.
func TestLayeredStore_CacheRestartRewarms(t *testing.T) {
	primary := NewPostgresStore(testPGDB(t), time.Hour)
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	cache := NewRedisStore(client, time.Hour)
	s := NewLayeredStore(primary, cache)

	// Pre-restart: both layers hold the session.
	s.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})
	if _, ok := cache.Get("sess-1"); !ok {
		t.Fatal("precondition: cache should hold the entry")
	}

	// Redis "restarts": wipe the dataset to model a fresh instance.
	mr.FlushAll()
	if _, ok := cache.Get("sess-1"); ok {
		t.Fatal("precondition: cache should be empty post-restart")
	}

	// Read through the Layered store: cache miss + primary hit + backfill.
	e, ok := s.Get("sess-1")
	if !ok || e.WorkerID != "w1" {
		t.Fatalf("Get after restart = (%+v, %v), want w1/true", e, ok)
	}
	if _, ok := cache.Get("sess-1"); !ok {
		t.Error("cache should be repopulated after the miss-through-read")
	}
}
