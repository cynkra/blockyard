package server

import (
	"testing"
	"time"
)

// Tests for Redis-specific edge cases that don't apply to the memory
// implementation: redis.Nil on empty Lua results, ghost entry prevention
// for all mutation paths.

func TestRedisWorkerMap_MarkDrainingEmptyOnNoMatch(t *testing.T) {
	m, _ := newRedisWorkerMap(t)
	m.Set("w1", ActiveWorker{AppID: "app1"})

	// Different app — Lua script returns empty table.
	ids := m.MarkDraining("nonexistent")
	if len(ids) != 0 {
		t.Errorf("MarkDraining(nonexistent) = %v, want empty", ids)
	}
}

func TestRedisWorkerMap_IdleWorkersEmptyOnNoMatch(t *testing.T) {
	m, _ := newRedisWorkerMap(t)
	m.Set("w1", ActiveWorker{AppID: "app1"})

	// No workers are idle.
	ids := m.IdleWorkers(5 * time.Minute)
	if len(ids) != 0 {
		t.Errorf("IdleWorkers() = %v, want empty", ids)
	}
}

func TestRedisWorkerMap_IdleWorkersEmptyOnEmptyMap(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	ids := m.IdleWorkers(5 * time.Minute)
	if len(ids) != 0 {
		t.Errorf("IdleWorkers() on empty map = %v, want empty", ids)
	}
}

func TestRedisWorkerMap_AppIDsEmptyOnEmptyMap(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	ids := m.AppIDs()
	if len(ids) != 0 {
		t.Errorf("AppIDs() on empty map = %v, want empty", ids)
	}
}

func TestRedisWorkerMap_GhostPrevention_ClearDraining(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	// ClearDraining on nonexistent worker must not create a key.
	m.ClearDraining("ghost")
	if _, ok := m.Get("ghost"); ok {
		t.Error("ClearDraining should not create ghost entry")
	}

	// Also test after delete.
	m.Set("w1", ActiveWorker{AppID: "app1", Draining: true})
	m.Delete("w1")
	m.ClearDraining("w1")
	if _, ok := m.Get("w1"); ok {
		t.Error("ClearDraining after delete should not create ghost entry")
	}
}

func TestRedisWorkerMap_GhostPrevention_SetIdleSince(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	m.SetIdleSince("ghost", time.Now())
	if _, ok := m.Get("ghost"); ok {
		t.Error("SetIdleSince should not create ghost entry")
	}

	m.Set("w1", ActiveWorker{AppID: "app1"})
	m.Delete("w1")
	m.SetIdleSince("w1", time.Now())
	if _, ok := m.Get("w1"); ok {
		t.Error("SetIdleSince after delete should not create ghost entry")
	}
}

func TestRedisWorkerMap_GhostPrevention_SetIdleSinceIfZero(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	m.SetIdleSinceIfZero("ghost", time.Now())
	if _, ok := m.Get("ghost"); ok {
		t.Error("SetIdleSinceIfZero should not create ghost entry")
	}

	m.Set("w1", ActiveWorker{AppID: "app1"})
	m.Delete("w1")
	m.SetIdleSinceIfZero("w1", time.Now())
	if _, ok := m.Get("w1"); ok {
		t.Error("SetIdleSinceIfZero after delete should not create ghost entry")
	}
}

func TestRedisWorkerMap_GhostPrevention_ClearIdleSince(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	if m.ClearIdleSince("ghost") {
		t.Error("ClearIdleSince on nonexistent should return false")
	}
	if _, ok := m.Get("ghost"); ok {
		t.Error("ClearIdleSince should not create ghost entry")
	}

	m.Set("w1", ActiveWorker{AppID: "app1"})
	m.SetIdleSince("w1", time.Now())
	m.Delete("w1")
	if m.ClearIdleSince("w1") {
		t.Error("ClearIdleSince after delete should return false")
	}
	if _, ok := m.Get("w1"); ok {
		t.Error("ClearIdleSince after delete should not create ghost entry")
	}
}
