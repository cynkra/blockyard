package session

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// TestRedisStoreConcurrentSetGet verifies that concurrent Set and Get
// operations don't corrupt data or panic.
func TestRedisStoreConcurrentSetGet(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	s := NewRedisStore(client, time.Hour)

	const workers = 10
	const sessionsPerWorker = 20

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < sessionsPerWorker; i++ {
				sid := fmt.Sprintf("sess-%d-%d", w, i)
				wid := fmt.Sprintf("worker-%d", w)
				s.Set(sid, Entry{WorkerID: wid, UserSub: "user", LastAccess: time.Now()})
				if e, ok := s.Get(sid); ok && e.WorkerID != wid {
					t.Errorf("Get(%s): WorkerID = %q, want %q", sid, e.WorkerID, wid)
				}
			}
		}(w)
	}
	wg.Wait()

	// Verify final state: every session should exist with the correct worker.
	for w := 0; w < workers; w++ {
		for i := 0; i < sessionsPerWorker; i++ {
			sid := fmt.Sprintf("sess-%d-%d", w, i)
			wid := fmt.Sprintf("worker-%d", w)
			e, ok := s.Get(sid)
			if !ok {
				t.Errorf("session %s missing after concurrent writes", sid)
			} else if e.WorkerID != wid {
				t.Errorf("session %s: WorkerID = %q, want %q", sid, e.WorkerID, wid)
			}
		}
	}
}

// TestRedisStoreConcurrentDeleteByWorker verifies that DeleteByWorker's
// Lua script produces correct counts when run concurrently.
func TestRedisStoreConcurrentDeleteByWorker(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	s := NewRedisStore(client, time.Hour)

	// Seed 5 sessions per worker for 4 workers.
	for w := 0; w < 4; w++ {
		for i := 0; i < 5; i++ {
			s.Set(fmt.Sprintf("s-%d-%d", w, i),
				Entry{WorkerID: fmt.Sprintf("w%d", w), LastAccess: time.Now()})
		}
	}

	// Concurrently delete by each worker.
	results := make([]int, 4)
	var wg sync.WaitGroup
	wg.Add(4)
	for w := 0; w < 4; w++ {
		go func(w int) {
			defer wg.Done()
			results[w] = s.DeleteByWorker(fmt.Sprintf("w%d", w))
		}(w)
	}
	wg.Wait()

	for w := 0; w < 4; w++ {
		if results[w] != 5 {
			t.Errorf("DeleteByWorker(w%d) = %d, want 5", w, results[w])
		}
	}

	// Nothing should remain.
	for w := 0; w < 4; w++ {
		if n := s.CountForWorker(fmt.Sprintf("w%d", w)); n != 0 {
			t.Errorf("CountForWorker(w%d) = %d after delete, want 0", w, n)
		}
	}
}

// TestRedisStoreConcurrentRerouteWorker verifies atomicity of reroute
// when multiple reroutes target disjoint worker sets concurrently.
func TestRedisStoreConcurrentRerouteWorker(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	s := NewRedisStore(client, time.Hour)

	const workers = 4
	const sessionsPerWorker = 10

	for w := 0; w < workers; w++ {
		for i := 0; i < sessionsPerWorker; i++ {
			s.Set(fmt.Sprintf("s-%d-%d", w, i),
				Entry{WorkerID: fmt.Sprintf("old-%d", w), LastAccess: time.Now()})
		}
	}

	var wg sync.WaitGroup
	wg.Add(workers)
	counts := make([]int, workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			counts[w] = s.RerouteWorker(fmt.Sprintf("old-%d", w), fmt.Sprintf("new-%d", w))
		}(w)
	}
	wg.Wait()

	for w := 0; w < workers; w++ {
		if counts[w] != sessionsPerWorker {
			t.Errorf("RerouteWorker(old-%d) = %d, want %d", w, counts[w], sessionsPerWorker)
		}
		if n := s.CountForWorker(fmt.Sprintf("new-%d", w)); n != sessionsPerWorker {
			t.Errorf("CountForWorker(new-%d) = %d, want %d", w, n, sessionsPerWorker)
		}
	}
}

// TestRedisStoreConcurrentTouch verifies that concurrent Touch calls
// don't lose updates or return incorrect results.
func TestRedisStoreConcurrentTouch(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	s := NewRedisStore(client, time.Hour)

	s.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now().Add(-time.Hour)})

	const goroutines = 20
	results := make([]bool, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = s.Touch("sess-1")
		}(i)
	}
	wg.Wait()

	for i, ok := range results {
		if !ok {
			t.Errorf("Touch goroutine %d returned false, want true", i)
		}
	}

	e, ok := s.Get("sess-1")
	if !ok {
		t.Fatal("session should still exist")
	}
	if e.WorkerID != "w1" {
		t.Errorf("WorkerID = %q, want %q", e.WorkerID, "w1")
	}
}
