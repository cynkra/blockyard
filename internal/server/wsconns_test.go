package server

import (
	"sync"
	"testing"
)

func TestWsConnCounterTryInc(t *testing.T) {
	c := NewWsConnCounter()

	if ok := c.TryInc("w1", 2); !ok {
		t.Error("TryInc 1/2 should succeed")
	}
	if ok := c.TryInc("w1", 2); !ok {
		t.Error("TryInc 2/2 should succeed")
	}
	if ok := c.TryInc("w1", 2); ok {
		t.Error("TryInc 3/2 should fail (over capacity)")
	}
	if n := c.Count("w1"); n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
}

func TestWsConnCounterDec(t *testing.T) {
	c := NewWsConnCounter()
	c.TryInc("w1", 10)
	c.TryInc("w1", 10)

	c.Dec("w1")
	if n := c.Count("w1"); n != 1 {
		t.Errorf("Count after first Dec = %d, want 1", n)
	}

	c.Dec("w1")
	if n := c.Count("w1"); n != 0 {
		t.Errorf("Count after second Dec = %d, want 0", n)
	}

	// Idempotent at zero.
	c.Dec("w1")
	if n := c.Count("w1"); n != 0 {
		t.Errorf("Count after third Dec = %d, want 0", n)
	}
}

func TestWsConnCounterCountForWorkers(t *testing.T) {
	c := NewWsConnCounter()
	c.TryInc("w1", 10)
	c.TryInc("w1", 10)
	c.TryInc("w2", 10)
	c.TryInc("w3", 10)

	if n := c.CountForWorkers([]string{"w1", "w2"}); n != 3 {
		t.Errorf("CountForWorkers([w1,w2]) = %d, want 3", n)
	}
	if n := c.CountForWorkers([]string{"w3"}); n != 1 {
		t.Errorf("CountForWorkers([w3]) = %d, want 1", n)
	}
	if n := c.CountForWorkers(nil); n != 0 {
		t.Errorf("CountForWorkers(nil) = %d, want 0", n)
	}
	if n := c.CountForWorkers([]string{"absent"}); n != 0 {
		t.Errorf("CountForWorkers([absent]) = %d, want 0", n)
	}
}

func TestWsConnCounterDeleteWorker(t *testing.T) {
	c := NewWsConnCounter()
	c.TryInc("w1", 10)
	c.TryInc("w1", 10)
	c.DeleteWorker("w1")
	if n := c.Count("w1"); n != 0 {
		t.Errorf("Count after DeleteWorker = %d, want 0", n)
	}
}

// TestWsConnCounterConcurrentTryInc checks that concurrent TryInc
// calls never exceed the configured maximum — the atomic
// check-and-increment is what blocks the over-capacity case.
func TestWsConnCounterConcurrentTryInc(t *testing.T) {
	c := NewWsConnCounter()

	const max = 5
	const goroutines = 100

	var wg sync.WaitGroup
	var successes int64
	var mu sync.Mutex

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.TryInc("w1", max) {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if successes != max {
		t.Errorf("concurrent TryInc: got %d successes, want exactly %d", successes, max)
	}
	if got := c.Count("w1"); got != max {
		t.Errorf("final Count = %d, want %d", got, max)
	}
}
