package server

import (
	"testing"
	"time"
)

// Tests that exercise Redis error paths using miniredis SetError.
// Every method should degrade gracefully (return zero-value / false / empty
// slice) and never panic.

func TestRedisWorkerMap_ErrorPaths(t *testing.T) {
	m, mr := newRedisWorkerMap(t)

	// Seed a worker so some operations have data to work with.
	m.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})

	// Make all commands return an error.
	mr.SetError("READONLY simulated failure")

	t.Run("Get", func(t *testing.T) {
		_, ok := m.Get("w1")
		if ok {
			t.Error("Get should return false when Redis errors")
		}
	})

	t.Run("Set", func(t *testing.T) {
		m.Set("w2", ActiveWorker{AppID: "app2"})
	})

	t.Run("Delete", func(t *testing.T) {
		m.Delete("w1")
	})

	t.Run("Count", func(t *testing.T) {
		if n := m.Count(); n != 0 {
			t.Errorf("Count = %d, want 0", n)
		}
	})

	t.Run("CountForApp", func(t *testing.T) {
		if n := m.CountForApp("app1"); n != 0 {
			t.Errorf("CountForApp = %d, want 0", n)
		}
	})

	t.Run("All", func(t *testing.T) {
		if ids := m.All(); len(ids) != 0 {
			t.Errorf("All = %v, want empty", ids)
		}
	})

	t.Run("ForApp", func(t *testing.T) {
		if ids := m.ForApp("app1"); len(ids) != 0 {
			t.Errorf("ForApp = %v, want empty", ids)
		}
	})

	t.Run("ForAppAvailable", func(t *testing.T) {
		if ids := m.ForAppAvailable("app1"); len(ids) != 0 {
			t.Errorf("ForAppAvailable = %v, want empty", ids)
		}
	})

	t.Run("MarkDraining", func(t *testing.T) {
		if ids := m.MarkDraining("app1"); len(ids) != 0 {
			t.Errorf("MarkDraining = %v, want empty", ids)
		}
	})

	t.Run("SetDraining", func(t *testing.T) {
		m.SetDraining("w1")
	})

	t.Run("ClearDraining", func(t *testing.T) {
		m.ClearDraining("w1")
	})

	t.Run("SetIdleSince", func(t *testing.T) {
		m.SetIdleSince("w1", time.Now())
	})

	t.Run("SetIdleSinceIfZero", func(t *testing.T) {
		m.SetIdleSinceIfZero("w1", time.Now())
	})

	t.Run("ClearIdleSince", func(t *testing.T) {
		if m.ClearIdleSince("w1") {
			t.Error("ClearIdleSince should return false on error")
		}
	})

	t.Run("IdleWorkers", func(t *testing.T) {
		if ids := m.IdleWorkers(5 * time.Minute); len(ids) != 0 {
			t.Errorf("IdleWorkers = %v, want empty", ids)
		}
	})

	t.Run("AppIDs", func(t *testing.T) {
		if ids := m.AppIDs(); len(ids) != 0 {
			t.Errorf("AppIDs = %v, want empty", ids)
		}
	})

	t.Run("IsDraining", func(t *testing.T) {
		if m.IsDraining("app1") {
			t.Error("IsDraining should return false on error")
		}
	})
}
