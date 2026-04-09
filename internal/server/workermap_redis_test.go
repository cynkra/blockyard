package server

import (
	"sort"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/redisstate"
)

func newRedisWorkerMap(t *testing.T) (*RedisWorkerMap, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	return NewRedisWorkerMap(client, "test-host"), mr
}

func TestRedisWorkerMapBasicOps(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	now := time.Now().Truncate(time.Second)
	m.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: now})

	w, ok := m.Get("w1")
	if !ok {
		t.Fatal("expected worker to exist")
	}
	if w.AppID != "app1" {
		t.Errorf("AppID = %q, want %q", w.AppID, "app1")
	}
	if w.BundleID != "b1" {
		t.Errorf("BundleID = %q, want %q", w.BundleID, "b1")
	}
	if !w.StartedAt.Equal(now) {
		t.Errorf("StartedAt = %v, want %v", w.StartedAt, now)
	}

	m.Delete("w1")
	_, ok = m.Get("w1")
	if ok {
		t.Error("expected worker to be deleted")
	}
}

func TestRedisWorkerMapCount(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	m.Set("w1", ActiveWorker{AppID: "app1"})
	m.Set("w2", ActiveWorker{AppID: "app1"})
	m.Set("w3", ActiveWorker{AppID: "app2"})

	if n := m.Count(); n != 3 {
		t.Errorf("Count() = %d, want 3", n)
	}
	if n := m.CountForApp("app1"); n != 2 {
		t.Errorf("CountForApp(app1) = %d, want 2", n)
	}
	if n := m.CountForApp("app2"); n != 1 {
		t.Errorf("CountForApp(app2) = %d, want 1", n)
	}
}

func TestRedisWorkerMapAll(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	m.Set("w1", ActiveWorker{AppID: "app1"})
	m.Set("w2", ActiveWorker{AppID: "app2"})

	ids := m.All()
	if len(ids) != 2 {
		t.Fatalf("All() returned %d, want 2", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen["w1"] || !seen["w2"] {
		t.Errorf("All() = %v, want w1 and w2", ids)
	}
}

func TestRedisWorkerMapForApp(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	m.Set("w1", ActiveWorker{AppID: "app1"})
	m.Set("w2", ActiveWorker{AppID: "app1"})
	m.Set("w3", ActiveWorker{AppID: "app2"})

	ids := m.ForApp("app1")
	if len(ids) != 2 {
		t.Errorf("ForApp(app1) returned %d, want 2", len(ids))
	}
}

func TestRedisWorkerMapForAppAvailable(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	m.Set("w1", ActiveWorker{AppID: "app1"})
	m.Set("w2", ActiveWorker{AppID: "app1", Draining: true})
	m.Set("w3", ActiveWorker{AppID: "app2"})

	ids := m.ForAppAvailable("app1")
	if len(ids) != 1 {
		t.Errorf("ForAppAvailable(app1) returned %d, want 1", len(ids))
	}
	if ids[0] != "w1" {
		t.Errorf("ForAppAvailable(app1) = %v, want [w1]", ids)
	}
}

func TestRedisWorkerMapDraining(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	m.Set("w1", ActiveWorker{AppID: "app1"})
	m.Set("w2", ActiveWorker{AppID: "app1"})
	m.Set("w3", ActiveWorker{AppID: "app2"})

	if m.IsDraining("app1") {
		t.Error("app1 should not be draining initially")
	}

	ids := m.MarkDraining("app1")
	if len(ids) != 2 {
		t.Errorf("MarkDraining returned %d, want 2", len(ids))
	}

	if !m.IsDraining("app1") {
		t.Error("app1 should be draining after MarkDraining")
	}
	if m.IsDraining("app2") {
		t.Error("app2 should not be draining")
	}

	// Test SetDraining on a single worker.
	m.Set("w4", ActiveWorker{AppID: "app2"})
	m.SetDraining("w4")
	w4, _ := m.Get("w4")
	if !w4.Draining {
		t.Error("w4 should be draining after SetDraining")
	}

	// Test ClearDraining.
	m.ClearDraining("w4")
	w4, _ = m.Get("w4")
	if w4.Draining {
		t.Error("w4 should not be draining after ClearDraining")
	}

	// SetDraining on deleted worker should not create ghost entry.
	m.Delete("w4")
	m.SetDraining("w4")
	_, ok := m.Get("w4")
	if ok {
		t.Error("SetDraining on deleted worker should not create ghost entry")
	}
}

func TestRedisWorkerMapIdleWorkers(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	now := time.Now().Truncate(time.Second)
	m.Set("w1", ActiveWorker{AppID: "app1"})
	m.Set("w2", ActiveWorker{AppID: "app1"})
	m.Set("w3", ActiveWorker{AppID: "app1", Draining: true})

	// Mark w1 as idle 10 minutes ago.
	m.SetIdleSince("w1", now.Add(-10*time.Minute))
	// Mark w3 (draining) as idle — should be excluded from idle list.
	m.SetIdleSince("w3", now.Add(-10*time.Minute))

	idle := m.IdleWorkers(5 * time.Minute)
	if len(idle) != 1 {
		t.Fatalf("IdleWorkers returned %d, want 1", len(idle))
	}
	if idle[0] != "w1" {
		t.Errorf("IdleWorkers = %v, want [w1]", idle)
	}
}

func TestRedisWorkerMapSetIdleSinceIfZero(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	now := time.Now().Truncate(time.Second)
	m.Set("w1", ActiveWorker{AppID: "app1"})

	// First call should set idle_since.
	m.SetIdleSinceIfZero("w1", now)
	w, _ := m.Get("w1")
	if w.IdleSince.IsZero() {
		t.Error("expected IdleSince to be set")
	}

	// Second call should not overwrite.
	later := now.Add(5 * time.Minute)
	m.SetIdleSinceIfZero("w1", later)
	w, _ = m.Get("w1")
	if w.IdleSince.Equal(later) {
		t.Error("SetIdleSinceIfZero should not overwrite existing value")
	}
}

func TestRedisWorkerMapClearIdleSince(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	m.Set("w1", ActiveWorker{AppID: "app1"})

	// Clearing when not idle should return false.
	if m.ClearIdleSince("w1") {
		t.Error("ClearIdleSince should return false when not idle")
	}

	m.SetIdleSince("w1", time.Now())
	if !m.ClearIdleSince("w1") {
		t.Error("ClearIdleSince should return true when was idle")
	}

	w, _ := m.Get("w1")
	if !w.IdleSince.IsZero() {
		t.Error("IdleSince should be zero after ClearIdleSince")
	}
}

func TestRedisWorkerMapAppIDs(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

	m.Set("w1", ActiveWorker{AppID: "app1"})
	m.Set("w2", ActiveWorker{AppID: "app1"})
	m.Set("w3", ActiveWorker{AppID: "app2"})

	ids := m.AppIDs()
	if len(ids) != 2 {
		t.Fatalf("AppIDs() returned %d, want 2", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen["app1"] || !seen["app2"] {
		t.Errorf("AppIDs() = %v, want app1 and app2", ids)
	}
}

func TestRedisWorkerMapRoundTrip(t *testing.T) {
	m, _ := newRedisWorkerMap(t)

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
}

// TestRedisWorkerMapWorkersForServerFiltersByServerID is the real
// exercise of the phase 3-8 filter: two RedisWorkerMaps with
// different server IDs share a miniredis, each registers workers,
// and WorkersForServer must return only the caller's own.
//
// This is the load-bearing invariant for drain.waitForIdle — during
// a same-host rolling update the old server must not count the new
// server's fresh sessions when deciding whether it is safe to tear
// down.
func TestRedisWorkerMapWorkersForServerFiltersByServerID(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisstate.TestClient(t, mr.Addr())
	old := NewRedisWorkerMap(client, "old-server")
	new := NewRedisWorkerMap(client, "new-server")

	// Old server registers two workers.
	old.Set("old-w1", ActiveWorker{AppID: "app1"})
	old.Set("old-w2", ActiveWorker{AppID: "app1"})
	// New server registers one worker on the same app — simulates
	// the overlap window during cutover.
	new.Set("new-w1", ActiveWorker{AppID: "app1"})

	// Each side queries through its own handle and must see only
	// its own workers.
	oldIDs := old.WorkersForServer("old-server")
	sort.Strings(oldIDs)
	if len(oldIDs) != 2 || oldIDs[0] != "old-w1" || oldIDs[1] != "old-w2" {
		t.Errorf("old server WorkersForServer = %v, want [old-w1 old-w2]", oldIDs)
	}

	newIDs := new.WorkersForServer("new-server")
	if len(newIDs) != 1 || newIDs[0] != "new-w1" {
		t.Errorf("new server WorkersForServer = %v, want [new-w1]", newIDs)
	}

	// Cross-queries (asking one side for the other side's server ID)
	// must also filter correctly — the query is parameterized, not
	// hardcoded to the struct field.
	cross := old.WorkersForServer("new-server")
	if len(cross) != 1 || cross[0] != "new-w1" {
		t.Errorf("cross-query WorkersForServer(new-server) via old handle = %v, want [new-w1]", cross)
	}

	// Unknown server ID returns empty (not all workers).
	if ids := old.WorkersForServer("ghost"); len(ids) != 0 {
		t.Errorf("WorkersForServer(ghost) = %v, want empty", ids)
	}
}
