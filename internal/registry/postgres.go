package registry

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"
)

// PostgresRegistry implements WorkerRegistry against the blockyard_workers
// table, making Postgres the source of truth for the worker address
// lookup (see #287, parent #262). The same table also backs
// server.PostgresWorkerMap; each store updates its own column set via
// upserts, so a production spawn that calls Workers.Set before
// Registry.Set converges to a full row.
//
// registryTTL mirrors the Redis TTL semantic: a worker whose last_heartbeat
// is older than registryTTL is treated as gone (Get returns not-found).
// The health poller calls Set on every successful probe, which bumps
// last_heartbeat back to now().
type PostgresRegistry struct {
	db          *sqlx.DB
	registryTTL time.Duration
}

func NewPostgresRegistry(db *sqlx.DB, registryTTL time.Duration) *PostgresRegistry {
	return &PostgresRegistry{db: db, registryTTL: registryTTL}
}

func (r *PostgresRegistry) Get(workerID string) (string, bool) {
	ctx := context.Background()
	var addr string
	var lastHeartbeat time.Time
	err := r.db.QueryRowxContext(ctx,
		`SELECT address, last_heartbeat
		 FROM blockyard_workers WHERE id = $1`,
		workerID,
	).Scan(&addr, &lastHeartbeat)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		slog.Error("postgres registry get", "worker_id", workerID, "error", err)
		return "", false
	}
	// Empty address means the row was created by PostgresWorkerMap alone
	// (no Registry.Set yet). Treat that as "no address known" rather
	// than handing out the empty string.
	if addr == "" {
		return "", false
	}
	if r.registryTTL > 0 && time.Since(lastHeartbeat) > r.registryTTL {
		return "", false
	}
	return addr, true
}

func (r *PostgresRegistry) Set(workerID, addr string) {
	ctx := context.Background()
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO blockyard_workers (id, address, last_heartbeat)
		 VALUES ($1, $2, now())
		 ON CONFLICT (id) DO UPDATE SET
		     address        = EXCLUDED.address,
		     last_heartbeat = EXCLUDED.last_heartbeat`,
		workerID, addr,
	)
	if err != nil {
		slog.Error("postgres registry set", "worker_id", workerID, "error", err)
	}
}

func (r *PostgresRegistry) Delete(workerID string) {
	ctx := context.Background()
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM blockyard_workers WHERE id = $1`, workerID,
	); err != nil {
		slog.Error("postgres registry delete", "worker_id", workerID, "error", err)
	}
}

var _ WorkerRegistry = (*PostgresRegistry)(nil)
