package main

import (
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/redisstate"
	"github.com/cynkra/blockyard/internal/registry"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
)

// sharedStateStores groups the registry, worker map, and session store
// constructed for a given shared-state mode. The two *Postgres* fields
// are exposed separately so callers can start the per-store background
// workers (RunExpiry, RunReaper) without type-asserting the interface
// values.
type sharedStateStores struct {
	Registry   registry.WorkerRegistry
	Workers    server.WorkerMap
	Sessions   session.Store
	PGSessions *session.PostgresStore
	PGWorkers  *server.PostgresWorkerMap
}

// buildSharedStateStores constructs the registry / worker map /
// session store triple for the given mode. Split out of main() so
// each mode's wiring is exercised directly by tests instead of by a
// full server boot. Callers do the post-construction bookkeeping
// (assigning onto *server.Server, spawning Postgres expiry/reaper
// goroutines) themselves.
//
// Unknown modes fall through to in-memory — config.Validate bounds
// the set to memory/redis/postgres/layered (plus "" which resolves to
// one of them), so the default arm only fires on a programming bug.
func buildSharedStateStores(
	mode config.SessionStoreMode,
	rc *redisstate.Client,
	pgDB *sqlx.DB,
	serverID string,
	registryTTL, idleTTL time.Duration,
) sharedStateStores {
	switch mode {
	case config.SessionStoreRedis:
		return sharedStateStores{
			Registry: registry.NewRedisRegistry(rc, registryTTL),
			Workers:  server.NewRedisWorkerMap(rc, serverID),
			Sessions: session.NewRedisStore(rc, idleTTL),
		}
	case config.SessionStorePostgres:
		pgW := server.NewPostgresWorkerMap(pgDB, serverID)
		pgS := session.NewPostgresStore(pgDB, idleTTL)
		return sharedStateStores{
			Registry:   registry.NewPostgresRegistry(pgDB, registryTTL),
			Workers:    pgW,
			Sessions:   pgS,
			PGSessions: pgS,
			PGWorkers:  pgW,
		}
	case config.SessionStoreLayered:
		pgW := server.NewPostgresWorkerMap(pgDB, serverID)
		pgS := session.NewPostgresStore(pgDB, idleTTL)
		return sharedStateStores{
			Registry: registry.NewLayeredRegistry(
				registry.NewPostgresRegistry(pgDB, registryTTL),
				registry.NewRedisRegistry(rc, registryTTL),
			),
			Workers: server.NewLayeredWorkerMap(
				pgW, server.NewRedisWorkerMap(rc, serverID),
			),
			Sessions: session.NewLayeredStore(
				pgS, session.NewRedisStore(rc, idleTTL),
			),
			PGSessions: pgS,
			PGWorkers:  pgW,
		}
	default:
		return sharedStateStores{
			Registry: registry.NewMemoryRegistry(),
			Workers:  server.NewMemoryWorkerMap(),
			Sessions: session.NewMemoryStore(),
		}
	}
}
