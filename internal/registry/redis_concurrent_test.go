package registry

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// TestRedisRegistryConcurrentSetGet verifies that concurrent Set and Get
// operations on different keys produce consistent results.
func TestRedisRegistryConcurrentSetGet(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	reg := NewRedisRegistry(client, 45*time.Second)

	const goroutines = 10
	const opsPerGoroutine = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				wid := fmt.Sprintf("w-%d-%d", g, i)
				addr := fmt.Sprintf("127.0.0.%d:%d", g, 3838+i)
				reg.Set(wid, addr)
				if got, ok := reg.Get(wid); ok && got != addr {
					t.Errorf("Get(%s) = %q, want %q", wid, got, addr)
				}
			}
		}(g)
	}
	wg.Wait()

	// Verify all entries survived.
	for g := 0; g < goroutines; g++ {
		for i := 0; i < opsPerGoroutine; i++ {
			wid := fmt.Sprintf("w-%d-%d", g, i)
			addr := fmt.Sprintf("127.0.0.%d:%d", g, 3838+i)
			got, ok := reg.Get(wid)
			if !ok {
				t.Errorf("registry entry %s missing", wid)
			} else if got != addr {
				t.Errorf("Get(%s) = %q, want %q", wid, got, addr)
			}
		}
	}
}

// TestRedisRegistryConcurrentSetDelete verifies that concurrent Set and
// Delete on disjoint keys don't interfere.
func TestRedisRegistryConcurrentSetDelete(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	reg := NewRedisRegistry(client, 45*time.Second)

	// Pre-populate entries to delete.
	for i := 0; i < 10; i++ {
		reg.Set(fmt.Sprintf("del-%d", i), "1.2.3.4:1000")
	}

	var wg sync.WaitGroup
	wg.Add(20)
	for i := 0; i < 10; i++ {
		go func(i int) {
			defer wg.Done()
			reg.Delete(fmt.Sprintf("del-%d", i))
		}(i)
		go func(i int) {
			defer wg.Done()
			reg.Set(fmt.Sprintf("new-%d", i), fmt.Sprintf("5.6.7.8:%d", 2000+i))
		}(i)
	}
	wg.Wait()

	for i := 0; i < 10; i++ {
		if _, ok := reg.Get(fmt.Sprintf("del-%d", i)); ok {
			t.Errorf("del-%d should be deleted", i)
		}
		if _, ok := reg.Get(fmt.Sprintf("new-%d", i)); !ok {
			t.Errorf("new-%d should exist", i)
		}
	}
}
