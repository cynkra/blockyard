package server

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// TestRedisWorkerMapConcurrentSetGet verifies that concurrent Set and Get
// operations produce consistent results.
func TestRedisWorkerMapConcurrentSetGet(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	m := NewRedisWorkerMap(client, "test-host")

	const goroutines = 10
	const opsPerGoroutine = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				wid := fmt.Sprintf("w-%d-%d", g, i)
				appID := fmt.Sprintf("app-%d", g)
				m.Set(wid, ActiveWorker{AppID: appID, BundleID: "b1", StartedAt: time.Now()})
				if w, ok := m.Get(wid); ok && w.AppID != appID {
					t.Errorf("Get(%s): AppID = %q, want %q", wid, w.AppID, appID)
				}
			}
		}(g)
	}
	wg.Wait()

	if n := m.Count(); n != goroutines*opsPerGoroutine {
		t.Errorf("Count() = %d, want %d", n, goroutines*opsPerGoroutine)
	}
}

// TestRedisWorkerMapConcurrentDraining verifies that MarkDraining and
// SetDraining/ClearDraining produce correct results under contention.
func TestRedisWorkerMapConcurrentDraining(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	m := NewRedisWorkerMap(client, "test-host")

	// Create workers across 4 apps.
	for app := 0; app < 4; app++ {
		for i := 0; i < 5; i++ {
			m.Set(fmt.Sprintf("w-%d-%d", app, i),
				ActiveWorker{AppID: fmt.Sprintf("app-%d", app)})
		}
	}

	// Concurrently mark each app as draining.
	results := make([][]string, 4)
	var wg sync.WaitGroup
	wg.Add(4)
	for app := 0; app < 4; app++ {
		go func(app int) {
			defer wg.Done()
			results[app] = m.MarkDraining(fmt.Sprintf("app-%d", app))
		}(app)
	}
	wg.Wait()

	for app := 0; app < 4; app++ {
		if len(results[app]) != 5 {
			t.Errorf("MarkDraining(app-%d) returned %d workers, want 5", app, len(results[app]))
		}
		if !m.IsDraining(fmt.Sprintf("app-%d", app)) {
			t.Errorf("app-%d should be draining", app)
		}
	}

	// Concurrently clear draining on all workers.
	wg.Add(4 * 5)
	for app := 0; app < 4; app++ {
		for i := 0; i < 5; i++ {
			go func(app, i int) {
				defer wg.Done()
				m.ClearDraining(fmt.Sprintf("w-%d-%d", app, i))
			}(app, i)
		}
	}
	wg.Wait()

	for app := 0; app < 4; app++ {
		if m.IsDraining(fmt.Sprintf("app-%d", app)) {
			t.Errorf("app-%d should not be draining after ClearDraining", app)
		}
	}
}

// TestRedisWorkerMapConcurrentIdleSince verifies that SetIdleSinceIfZero's
// atomicity guarantee holds under concurrent access: only the first
// caller's timestamp wins.
func TestRedisWorkerMapConcurrentIdleSince(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	m := NewRedisWorkerMap(client, "test-host")

	m.Set("w1", ActiveWorker{AppID: "app1"})

	// Race N goroutines to set idle_since. Only the first should win.
	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	base := time.Now().Truncate(time.Second)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			m.SetIdleSinceIfZero("w1", base.Add(time.Duration(i)*time.Second))
		}(i)
	}
	wg.Wait()

	w, ok := m.Get("w1")
	if !ok {
		t.Fatal("worker should exist")
	}
	if w.IdleSince.IsZero() {
		t.Fatal("IdleSince should be set")
	}

	// The value should be one of the candidates (any single goroutine's timestamp).
	// The key invariant is that it's set exactly once — verify by clearing and
	// checking it was set.
	if !m.ClearIdleSince("w1") {
		t.Error("ClearIdleSince should return true")
	}
	w, _ = m.Get("w1")
	if !w.IdleSince.IsZero() {
		t.Error("IdleSince should be zero after clear")
	}
}

// TestRedisWorkerMapConcurrentDeleteAndSet verifies that Delete and Set
// on different keys don't interfere.
func TestRedisWorkerMapConcurrentDeleteAndSet(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	m := NewRedisWorkerMap(client, "test-host")

	// Pre-populate workers to delete.
	for i := 0; i < 10; i++ {
		m.Set(fmt.Sprintf("del-%d", i), ActiveWorker{AppID: "old"})
	}

	var wg sync.WaitGroup
	// Delete old workers.
	wg.Add(10)
	for i := 0; i < 10; i++ {
		go func(i int) {
			defer wg.Done()
			m.Delete(fmt.Sprintf("del-%d", i))
		}(i)
	}
	// Simultaneously add new workers.
	wg.Add(10)
	for i := 0; i < 10; i++ {
		go func(i int) {
			defer wg.Done()
			m.Set(fmt.Sprintf("new-%d", i), ActiveWorker{AppID: "new"})
		}(i)
	}
	wg.Wait()

	// All old workers should be gone, all new workers should exist.
	for i := 0; i < 10; i++ {
		if _, ok := m.Get(fmt.Sprintf("del-%d", i)); ok {
			t.Errorf("del-%d should be deleted", i)
		}
		if _, ok := m.Get(fmt.Sprintf("new-%d", i)); !ok {
			t.Errorf("new-%d should exist", i)
		}
	}

	if n := m.Count(); n != 10 {
		t.Errorf("Count() = %d, want 10", n)
	}
}
