package server

import (
	"sort"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// workerMapFactory returns a fresh WorkerMap for each test.
type workerMapFactory func(t *testing.T) WorkerMap

func workerMapImplementations(t *testing.T) map[string]workerMapFactory {
	t.Helper()
	return map[string]workerMapFactory{
		"Memory": func(t *testing.T) WorkerMap {
			t.Helper()
			return NewMemoryWorkerMap()
		},
		"Redis": func(t *testing.T) WorkerMap {
			t.Helper()
			mr := miniredis.RunT(t)
			client := redisstate.TestClient(t, mr.Addr())
			return NewRedisWorkerMap(client, "test-host")
		},
	}
}

func TestWorkerMapConformance_EmptyState(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)

			if n := m.Count(); n != 0 {
				t.Errorf("Count() = %d, want 0", n)
			}
			if n := m.CountForApp("app1"); n != 0 {
				t.Errorf("CountForApp(app1) = %d, want 0", n)
			}
			if ids := m.All(); len(ids) != 0 {
				t.Errorf("All() = %v, want empty", ids)
			}
			if ids := m.ForApp("app1"); len(ids) != 0 {
				t.Errorf("ForApp(app1) = %v, want empty", ids)
			}
			if ids := m.ForAppAvailable("app1"); len(ids) != 0 {
				t.Errorf("ForAppAvailable(app1) = %v, want empty", ids)
			}
			if ids := m.MarkDraining("app1"); len(ids) != 0 {
				t.Errorf("MarkDraining(app1) = %v, want empty", ids)
			}
			if ids := m.IdleWorkers(5 * time.Minute); len(ids) != 0 {
				t.Errorf("IdleWorkers() = %v, want empty", ids)
			}
			if ids := m.AppIDs(); len(ids) != 0 {
				t.Errorf("AppIDs() = %v, want empty", ids)
			}
			if m.IsDraining("app1") {
				t.Error("IsDraining(app1) = true, want false")
			}
		})
	}
}

func TestWorkerMapConformance_GetMissing(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			_, ok := m.Get("nonexistent")
			if ok {
				t.Error("Get(nonexistent) should return false")
			}
		})
	}
}

func TestWorkerMapConformance_SetOverwrite(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			now := time.Now().Truncate(time.Second)

			m.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: now})
			m.Set("w1", ActiveWorker{AppID: "app2", BundleID: "b2", StartedAt: now})

			w, ok := m.Get("w1")
			if !ok {
				t.Fatal("expected worker to exist")
			}
			if w.AppID != "app2" {
				t.Errorf("AppID = %q, want %q after overwrite", w.AppID, "app2")
			}
			if w.BundleID != "b2" {
				t.Errorf("BundleID = %q, want %q after overwrite", w.BundleID, "b2")
			}
			// Overwrite should not create a duplicate.
			if n := m.Count(); n != 1 {
				t.Errorf("Count() = %d, want 1", n)
			}
		})
	}
}

func TestWorkerMapConformance_DeleteNonexistent(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			// Should not panic or error.
			m.Delete("nonexistent")
			if n := m.Count(); n != 0 {
				t.Errorf("Count() = %d, want 0", n)
			}
		})
	}
}

func TestWorkerMapConformance_CountForApp(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)

			m.Set("w1", ActiveWorker{AppID: "app1"})
			m.Set("w2", ActiveWorker{AppID: "app1"})
			m.Set("w3", ActiveWorker{AppID: "app2"})

			if n := m.CountForApp("app1"); n != 2 {
				t.Errorf("CountForApp(app1) = %d, want 2", n)
			}
			if n := m.CountForApp("nonexistent"); n != 0 {
				t.Errorf("CountForApp(nonexistent) = %d, want 0", n)
			}
		})
	}
}

func TestWorkerMapConformance_ForAppNoMatch(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			m.Set("w1", ActiveWorker{AppID: "app1"})

			if ids := m.ForApp("nonexistent"); len(ids) != 0 {
				t.Errorf("ForApp(nonexistent) = %v, want empty", ids)
			}
		})
	}
}

func TestWorkerMapConformance_ForAppAvailableAllDraining(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			m.Set("w1", ActiveWorker{AppID: "app1", Draining: true})
			m.Set("w2", ActiveWorker{AppID: "app1", Draining: true})

			if ids := m.ForAppAvailable("app1"); len(ids) != 0 {
				t.Errorf("ForAppAvailable(app1) = %v, want empty when all draining", ids)
			}
		})
	}
}

func TestWorkerMapConformance_DrainingLifecycle(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			m.Set("w1", ActiveWorker{AppID: "app1"})
			m.Set("w2", ActiveWorker{AppID: "app1"})
			m.Set("w3", ActiveWorker{AppID: "app2"})

			// Initially not draining.
			if m.IsDraining("app1") {
				t.Error("app1 should not be draining initially")
			}

			// MarkDraining returns affected IDs.
			ids := m.MarkDraining("app1")
			sort.Strings(ids)
			if len(ids) != 2 {
				t.Fatalf("MarkDraining(app1) returned %d, want 2", len(ids))
			}

			if !m.IsDraining("app1") {
				t.Error("app1 should be draining")
			}
			if m.IsDraining("app2") {
				t.Error("app2 should not be draining")
			}

			// ForAppAvailable excludes draining workers.
			avail := m.ForAppAvailable("app1")
			if len(avail) != 0 {
				t.Errorf("ForAppAvailable(app1) = %v, want empty", avail)
			}

			// ClearDraining on individual worker.
			m.ClearDraining("w1")
			w1, _ := m.Get("w1")
			if w1.Draining {
				t.Error("w1 should not be draining after ClearDraining")
			}

			// app1 still has w2 draining.
			if !m.IsDraining("app1") {
				t.Error("app1 should still be draining (w2)")
			}

			// SetDraining on individual worker.
			m.SetDraining("w3")
			w3, _ := m.Get("w3")
			if !w3.Draining {
				t.Error("w3 should be draining after SetDraining")
			}
		})
	}
}

func TestWorkerMapConformance_MarkDrainingNoMatch(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			m.Set("w1", ActiveWorker{AppID: "app1"})

			ids := m.MarkDraining("nonexistent")
			if len(ids) != 0 {
				t.Errorf("MarkDraining(nonexistent) = %v, want empty", ids)
			}
		})
	}
}

func TestWorkerMapConformance_SetDrainingOnMissing(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			// Should not create a ghost entry.
			m.SetDraining("nonexistent")
			_, ok := m.Get("nonexistent")
			if ok {
				t.Error("SetDraining on missing worker should not create entry")
			}
		})
	}
}

func TestWorkerMapConformance_ClearDrainingOnMissing(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			// Should not create a ghost entry.
			m.ClearDraining("nonexistent")
			_, ok := m.Get("nonexistent")
			if ok {
				t.Error("ClearDraining on missing worker should not create entry")
			}
		})
	}
}

func TestWorkerMapConformance_IdleLifecycle(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			now := time.Now().Truncate(time.Second)
			m.Set("w1", ActiveWorker{AppID: "app1"})

			// Not idle initially.
			w, _ := m.Get("w1")
			if !w.IdleSince.IsZero() {
				t.Error("IdleSince should be zero initially")
			}
			if m.ClearIdleSince("w1") {
				t.Error("ClearIdleSince should return false when not idle")
			}

			// SetIdleSince marks idle.
			m.SetIdleSince("w1", now.Add(-10*time.Minute))
			w, _ = m.Get("w1")
			if w.IdleSince.IsZero() {
				t.Error("IdleSince should be set")
			}

			// IdleWorkers returns it.
			idle := m.IdleWorkers(5 * time.Minute)
			if len(idle) != 1 || idle[0] != "w1" {
				t.Errorf("IdleWorkers(5m) = %v, want [w1]", idle)
			}

			// Not yet idle for longer timeout.
			idle = m.IdleWorkers(15 * time.Minute)
			if len(idle) != 0 {
				t.Errorf("IdleWorkers(15m) = %v, want empty", idle)
			}

			// ClearIdleSince returns true and resets.
			if !m.ClearIdleSince("w1") {
				t.Error("ClearIdleSince should return true when was idle")
			}
			w, _ = m.Get("w1")
			if !w.IdleSince.IsZero() {
				t.Error("IdleSince should be zero after clear")
			}
		})
	}
}

func TestWorkerMapConformance_SetIdleSinceIfZeroSemantics(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			now := time.Now().Truncate(time.Second)
			m.Set("w1", ActiveWorker{AppID: "app1"})

			// First call sets it.
			m.SetIdleSinceIfZero("w1", now)
			w, _ := m.Get("w1")
			if !w.IdleSince.Equal(now) {
				t.Errorf("IdleSince = %v, want %v", w.IdleSince, now)
			}

			// Second call does not overwrite.
			later := now.Add(5 * time.Minute)
			m.SetIdleSinceIfZero("w1", later)
			w, _ = m.Get("w1")
			if !w.IdleSince.Equal(now) {
				t.Errorf("IdleSince = %v, want %v (should not overwrite)", w.IdleSince, now)
			}
		})
	}
}

func TestWorkerMapConformance_SetIdleSinceOnMissing(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			// Should not create ghost entries.
			m.SetIdleSince("nonexistent", time.Now())
			_, ok := m.Get("nonexistent")
			if ok {
				t.Error("SetIdleSince on missing worker should not create entry")
			}

			m.SetIdleSinceIfZero("nonexistent", time.Now())
			_, ok = m.Get("nonexistent")
			if ok {
				t.Error("SetIdleSinceIfZero on missing worker should not create entry")
			}
		})
	}
}

func TestWorkerMapConformance_ClearIdleSinceOnMissing(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			if m.ClearIdleSince("nonexistent") {
				t.Error("ClearIdleSince on missing worker should return false")
			}
		})
	}
}

func TestWorkerMapConformance_IdleWorkersExcludesDraining(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)
			now := time.Now().Truncate(time.Second)

			m.Set("w1", ActiveWorker{AppID: "app1"})
			m.Set("w2", ActiveWorker{AppID: "app1", Draining: true})
			m.SetIdleSince("w1", now.Add(-10*time.Minute))
			m.SetIdleSince("w2", now.Add(-10*time.Minute))

			idle := m.IdleWorkers(5 * time.Minute)
			if len(idle) != 1 || idle[0] != "w1" {
				t.Errorf("IdleWorkers should exclude draining, got %v", idle)
			}
		})
	}
}

func TestWorkerMapConformance_AppIDsDedup(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)

			m.Set("w1", ActiveWorker{AppID: "app1"})
			m.Set("w2", ActiveWorker{AppID: "app1"})
			m.Set("w3", ActiveWorker{AppID: "app2"})

			ids := m.AppIDs()
			sort.Strings(ids)
			if len(ids) != 2 {
				t.Fatalf("AppIDs() returned %d, want 2", len(ids))
			}
			if ids[0] != "app1" || ids[1] != "app2" {
				t.Errorf("AppIDs() = %v, want [app1 app2]", ids)
			}
		})
	}
}

func TestWorkerMapConformance_RoundTrip(t *testing.T) {
	for name, factory := range workerMapImplementations(t) {
		t.Run(name, func(t *testing.T) {
			m := factory(t)

			now := time.Now().Truncate(time.Second)
			idle := now.Add(-5 * time.Minute)
			w := ActiveWorker{
				AppID:     "app1",
				BundleID:  "bundle-abc",
				Draining:  true,
				IdleSince: idle,
				StartedAt: now,
			}
			m.Set("w1", w)

			got, ok := m.Get("w1")
			if !ok {
				t.Fatal("expected worker to exist")
			}
			if got.AppID != w.AppID {
				t.Errorf("AppID = %q, want %q", got.AppID, w.AppID)
			}
			if got.BundleID != w.BundleID {
				t.Errorf("BundleID = %q, want %q", got.BundleID, w.BundleID)
			}
			if got.Draining != w.Draining {
				t.Errorf("Draining = %v, want %v", got.Draining, w.Draining)
			}
			if !got.IdleSince.Equal(idle) {
				t.Errorf("IdleSince = %v, want %v", got.IdleSince, idle)
			}
			if !got.StartedAt.Equal(now) {
				t.Errorf("StartedAt = %v, want %v", got.StartedAt, now)
			}
		})
	}
}
