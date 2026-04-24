package main

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/redisstate"
	"github.com/cynkra/blockyard/internal/registry"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
)

// buildSharedStateStores is the single place where the SessionStore
// mode chooses concrete registry / worker map / session store
// implementations. Each mode's branch is validated here so adding a
// new backend triple has a failure pattern that doesn't require a
// full server boot.

func TestBuildSharedStateStores_Memory(t *testing.T) {
	stores := buildSharedStateStores(
		config.SessionStoreMemory,
		nil, nil, "srv-A",
		30*time.Second, 2*time.Minute,
	)

	if _, ok := stores.Registry.(*registry.MemoryRegistry); !ok {
		t.Errorf("Registry = %T, want *MemoryRegistry", stores.Registry)
	}
	if _, ok := stores.Workers.(*server.MemoryWorkerMap); !ok {
		t.Errorf("Workers = %T, want *MemoryWorkerMap", stores.Workers)
	}
	if _, ok := stores.Sessions.(*session.MemoryStore); !ok {
		t.Errorf("Sessions = %T, want *MemoryStore", stores.Sessions)
	}
	if stores.PGSessions != nil {
		t.Error("PGSessions should be nil in memory mode")
	}
	if stores.PGWorkers != nil {
		t.Error("PGWorkers should be nil in memory mode")
	}
}

// Unknown modes must not panic; buildSharedStateStores treats them as
// memory (config.Validate gates the input).
func TestBuildSharedStateStores_UnknownDefaultsToMemory(t *testing.T) {
	stores := buildSharedStateStores(
		config.SessionStoreMode("unexpected"),
		nil, nil, "srv-A",
		30*time.Second, 2*time.Minute,
	)

	if _, ok := stores.Registry.(*registry.MemoryRegistry); !ok {
		t.Errorf("Registry = %T, want *MemoryRegistry", stores.Registry)
	}
	if _, ok := stores.Workers.(*server.MemoryWorkerMap); !ok {
		t.Errorf("Workers = %T, want *MemoryWorkerMap", stores.Workers)
	}
	if _, ok := stores.Sessions.(*session.MemoryStore); !ok {
		t.Errorf("Sessions = %T, want *MemoryStore", stores.Sessions)
	}
}

func TestBuildSharedStateStores_Redis(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redisstate.TestClient(t, mr.Addr())

	stores := buildSharedStateStores(
		config.SessionStoreRedis,
		rc, nil, "srv-A",
		30*time.Second, 2*time.Minute,
	)

	if _, ok := stores.Registry.(*registry.RedisRegistry); !ok {
		t.Errorf("Registry = %T, want *RedisRegistry", stores.Registry)
	}
	if _, ok := stores.Workers.(*server.RedisWorkerMap); !ok {
		t.Errorf("Workers = %T, want *RedisWorkerMap", stores.Workers)
	}
	if _, ok := stores.Sessions.(*session.RedisStore); !ok {
		t.Errorf("Sessions = %T, want *RedisStore", stores.Sessions)
	}
	if stores.PGSessions != nil {
		t.Error("PGSessions should be nil in redis mode")
	}
	if stores.PGWorkers != nil {
		t.Error("PGWorkers should be nil in redis mode")
	}
}

// Postgres mode wiring is exercised by type assertions; the
// Postgres{Registry,WorkerMap,Store} constructors only capture the
// *sqlx.DB pointer, so passing nil here is safe. The database is
// never dereferenced by the wiring itself — only by later method
// calls.
func TestBuildSharedStateStores_Postgres(t *testing.T) {
	stores := buildSharedStateStores(
		config.SessionStorePostgres,
		nil, nil, "srv-A",
		30*time.Second, 2*time.Minute,
	)

	if _, ok := stores.Registry.(*registry.PostgresRegistry); !ok {
		t.Errorf("Registry = %T, want *PostgresRegistry", stores.Registry)
	}
	if _, ok := stores.Workers.(*server.PostgresWorkerMap); !ok {
		t.Errorf("Workers = %T, want *PostgresWorkerMap", stores.Workers)
	}
	if _, ok := stores.Sessions.(*session.PostgresStore); !ok {
		t.Errorf("Sessions = %T, want *PostgresStore", stores.Sessions)
	}
	if stores.PGSessions == nil {
		t.Error("PGSessions should be non-nil so main() can start RunExpiry")
	}
	if stores.PGWorkers == nil {
		t.Error("PGWorkers should be non-nil so main() can start RunReaper")
	}
	// The interface and the *Postgres* fields must point at the same
	// instance — main() uses the latter to start RunExpiry/RunReaper
	// on the value exposed as the former.
	if stores.Sessions != session.Store(stores.PGSessions) {
		t.Error("Sessions and PGSessions must be the same instance")
	}
	if stores.Workers != server.WorkerMap(stores.PGWorkers) {
		t.Error("Workers and PGWorkers must be the same instance")
	}
}

func TestBuildSharedStateStores_Layered(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redisstate.TestClient(t, mr.Addr())

	stores := buildSharedStateStores(
		config.SessionStoreLayered,
		rc, nil, "srv-A",
		30*time.Second, 2*time.Minute,
	)

	if _, ok := stores.Registry.(*registry.LayeredRegistry); !ok {
		t.Errorf("Registry = %T, want *LayeredRegistry", stores.Registry)
	}
	if _, ok := stores.Workers.(*server.LayeredWorkerMap); !ok {
		t.Errorf("Workers = %T, want *LayeredWorkerMap", stores.Workers)
	}
	if _, ok := stores.Sessions.(*session.LayeredStore); !ok {
		t.Errorf("Sessions = %T, want *LayeredStore", stores.Sessions)
	}
	// Layered mode still exposes PGSessions/PGWorkers so main() can
	// drive the Postgres-only background loops (RunExpiry, RunReaper)
	// against the primary tier — the Redis tier is a cache and needs
	// neither.
	if stores.PGSessions == nil {
		t.Error("PGSessions should be non-nil in layered mode")
	}
	if stores.PGWorkers == nil {
		t.Error("PGWorkers should be non-nil in layered mode")
	}
}
