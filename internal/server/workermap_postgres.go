package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"
)

// PostgresWorkerMap implements WorkerMap against the blockyard_workers
// table, making Postgres the source of truth for worker metadata (see
// #287, parent #262). The same table also backs registry.PostgresRegistry
// — each store upserts its own column subset so a production spawn
// that calls Workers.Set before Registry.Set converges to a full row.
//
// idle_since is stored as NULL when the worker is not idle (mapped to
// Go's zero-value time.Time), mirroring the MemoryWorkerMap semantic
// used throughout the codebase.
type PostgresWorkerMap struct {
	db       *sqlx.DB
	serverID string
}

func NewPostgresWorkerMap(db *sqlx.DB, serverID string) *PostgresWorkerMap {
	return &PostgresWorkerMap{db: db, serverID: serverID}
}

func (m *PostgresWorkerMap) Get(id string) (ActiveWorker, bool) {
	ctx := context.Background()
	var w ActiveWorker
	var idleSince sql.NullTime
	err := m.db.QueryRowxContext(ctx,
		`SELECT app_id, bundle_id, draining, idle_since, started_at
		 FROM blockyard_workers WHERE id = $1`,
		id,
	).Scan(&w.AppID, &w.BundleID, &w.Draining, &idleSince, &w.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ActiveWorker{}, false
	}
	if err != nil {
		slog.Error("postgres worker get", "worker_id", id, "error", err)
		return ActiveWorker{}, false
	}
	if idleSince.Valid {
		w.IdleSince = idleSince.Time
	}
	return w, true
}

func (m *PostgresWorkerMap) Set(id string, w ActiveWorker) {
	ctx := context.Background()
	var idleSince sql.NullTime
	if !w.IdleSince.IsZero() {
		idleSince = sql.NullTime{Time: w.IdleSince, Valid: true}
	}
	startedAt := w.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO blockyard_workers
		     (id, app_id, bundle_id, server_id, draining, idle_since, started_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (id) DO UPDATE SET
		     app_id     = EXCLUDED.app_id,
		     bundle_id  = EXCLUDED.bundle_id,
		     server_id  = EXCLUDED.server_id,
		     draining   = EXCLUDED.draining,
		     idle_since = EXCLUDED.idle_since,
		     started_at = EXCLUDED.started_at`,
		id, w.AppID, w.BundleID, m.serverID, w.Draining, idleSince, startedAt,
	)
	if err != nil {
		slog.Error("postgres worker set", "worker_id", id, "error", err)
	}
}

func (m *PostgresWorkerMap) Delete(id string) {
	ctx := context.Background()
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM blockyard_workers WHERE id = $1`, id,
	); err != nil {
		slog.Error("postgres worker delete", "worker_id", id, "error", err)
	}
}

// Count returns the total number of tracked workers. Rows created by
// registry.PostgresRegistry alone (no Workers.Set ever called) still
// count — a registered address is a tracked worker.
func (m *PostgresWorkerMap) Count() int {
	ctx := context.Background()
	var n int
	if err := m.db.QueryRowxContext(ctx,
		`SELECT COUNT(*) FROM blockyard_workers`,
	).Scan(&n); err != nil {
		slog.Error("postgres worker count", "error", err)
		return 0
	}
	return n
}

func (m *PostgresWorkerMap) CountForApp(appID string) int {
	ctx := context.Background()
	var n int
	if err := m.db.QueryRowxContext(ctx,
		`SELECT COUNT(*) FROM blockyard_workers WHERE app_id = $1`, appID,
	).Scan(&n); err != nil {
		slog.Error("postgres worker count for app",
			"app_id", appID, "error", err)
		return 0
	}
	return n
}

func (m *PostgresWorkerMap) All() []string {
	return m.selectIDs(`SELECT id FROM blockyard_workers`)
}

func (m *PostgresWorkerMap) ForApp(appID string) []string {
	return m.selectIDs(
		`SELECT id FROM blockyard_workers WHERE app_id = $1`, appID,
	)
}

func (m *PostgresWorkerMap) ForAppAvailable(appID string) []string {
	return m.selectIDs(
		`SELECT id FROM blockyard_workers
		 WHERE app_id = $1 AND draining = false`, appID,
	)
}

// MarkDraining sets draining on every worker for the given app and
// returns the affected worker IDs. Implemented as a single UPDATE ...
// RETURNING so the read-modify-write stays atomic.
func (m *PostgresWorkerMap) MarkDraining(appID string) []string {
	ctx := context.Background()
	rows, err := m.db.QueryxContext(ctx,
		`UPDATE blockyard_workers SET draining = true
		 WHERE app_id = $1 RETURNING id`, appID,
	)
	if err != nil {
		slog.Error("postgres worker mark draining",
			"app_id", appID, "error", err)
		return nil
	}
	defer rows.Close()
	return collectIDs(rows, "mark draining")
}

// SetDraining flips draining on a single worker. The WHERE guard
// matches the "must not create ghost entry" conformance test — if the
// row doesn't exist, nothing is inserted.
func (m *PostgresWorkerMap) SetDraining(workerID string) {
	m.execUpdate(
		`UPDATE blockyard_workers SET draining = true WHERE id = $1`,
		"set draining", workerID,
	)
}

func (m *PostgresWorkerMap) ClearDraining(workerID string) {
	m.execUpdate(
		`UPDATE blockyard_workers SET draining = false WHERE id = $1`,
		"clear draining", workerID,
	)
}

func (m *PostgresWorkerMap) SetIdleSince(workerID string, t time.Time) {
	m.execUpdate(
		`UPDATE blockyard_workers SET idle_since = $2 WHERE id = $1`,
		"set idle since", workerID, t,
	)
}

// SetIdleSinceIfZero sets idle_since only when it's currently NULL
// (the zero-value representation). Matches the MemoryWorkerMap
// semantic used to avoid resetting the timer on repeated ticks.
func (m *PostgresWorkerMap) SetIdleSinceIfZero(workerID string, t time.Time) {
	m.execUpdate(
		`UPDATE blockyard_workers SET idle_since = $2
		 WHERE id = $1 AND idle_since IS NULL`,
		"set idle since if zero", workerID, t,
	)
}

// ClearIdleSince resets idle_since to NULL and reports whether the
// worker was previously idle. Done in a single UPDATE so the
// read-modify-write is race-free.
func (m *PostgresWorkerMap) ClearIdleSince(workerID string) bool {
	ctx := context.Background()
	res, err := m.db.ExecContext(ctx,
		`UPDATE blockyard_workers SET idle_since = NULL
		 WHERE id = $1 AND idle_since IS NOT NULL`, workerID,
	)
	if err != nil {
		slog.Error("postgres worker clear idle since",
			"worker_id", workerID, "error", err)
		return false
	}
	n, err := res.RowsAffected()
	if err != nil {
		slog.Error("postgres worker clear idle since rows",
			"worker_id", workerID, "error", err)
		return false
	}
	return n > 0
}

// IdleWorkers returns workers idle longer than timeout, excluding
// draining workers (they're on their own lifecycle).
func (m *PostgresWorkerMap) IdleWorkers(timeout time.Duration) []string {
	cutoff := time.Now().Add(-timeout)
	return m.selectIDs(
		`SELECT id FROM blockyard_workers
		 WHERE idle_since IS NOT NULL
		   AND idle_since <= $1
		   AND draining = false`, cutoff,
	)
}

// AppIDs returns the deduplicated set of app_ids with at least one
// tracked worker. Filters out the empty-string default so rows created
// by registry.PostgresRegistry alone don't leak a bogus "" entry.
func (m *PostgresWorkerMap) AppIDs() []string {
	return m.selectIDs(
		`SELECT DISTINCT app_id FROM blockyard_workers WHERE app_id <> ''`,
	)
}

func (m *PostgresWorkerMap) IsDraining(appID string) bool {
	ctx := context.Background()
	var exists bool
	if err := m.db.QueryRowxContext(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM blockyard_workers
		     WHERE app_id = $1 AND draining = true
		 )`, appID,
	).Scan(&exists); err != nil {
		slog.Error("postgres worker is draining",
			"app_id", appID, "error", err)
		return false
	}
	return exists
}

func (m *PostgresWorkerMap) WorkersForServer(serverID string) []string {
	return m.selectIDs(
		`SELECT id FROM blockyard_workers WHERE server_id = $1`, serverID,
	)
}

func (m *PostgresWorkerMap) selectIDs(query string, args ...any) []string {
	ctx := context.Background()
	rows, err := m.db.QueryxContext(ctx, query, args...)
	if err != nil {
		slog.Error("postgres worker select", "error", err)
		return nil
	}
	defer rows.Close()
	return collectIDs(rows, "select")
}

func collectIDs(rows *sqlx.Rows, op string) []string {
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			slog.Error("postgres worker scan", "op", op, "error", err)
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func (m *PostgresWorkerMap) execUpdate(query, op string, args ...any) {
	ctx := context.Background()
	if _, err := m.db.ExecContext(ctx, query, args...); err != nil {
		slog.Error("postgres worker "+op, "args", args, "error", err)
	}
}

// RunReaper deletes blockyard_workers rows whose last_heartbeat has been
// stale for longer than `threshold`, every `interval`. Blocks until
// ctx is cancelled. Caller runs it in a goroutine.
//
// Motivation: Redis workermap entries had implicit TTLs that cleaned up
// after a pod died without graceful shutdown. Postgres doesn't expire
// rows, so without this sweep a crashed pod's workers linger in the
// shared blockyard_workers table — Registry.Get hides them via its own
// last_heartbeat filter, but WorkerMap's Count / ForApp / IdleWorkers /
// AppIDs would keep reporting ghosts and skew scheduler decisions.
//
// Pick a threshold well above registryTTL so a transient network blip
// (heartbeat writes briefly blocked) doesn't reap a worker the health
// poller will resume updating seconds later. cmd/blockyard/main.go
// uses 5 × registryTTL, giving 60 s of recovery slack on top of the
// 15 s "Registry considers dead" signal with default health intervals.
func (m *PostgresWorkerMap) RunReaper(ctx context.Context, threshold, interval time.Duration) {
	if threshold <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reapStale(ctx, threshold)
		}
	}
}

func (m *PostgresWorkerMap) reapStale(ctx context.Context, threshold time.Duration) {
	cutoff := time.Now().Add(-threshold)
	res, err := m.db.ExecContext(ctx,
		`DELETE FROM blockyard_workers WHERE last_heartbeat < $1`, cutoff,
	)
	if err != nil {
		slog.Error("postgres worker reaper", "error", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		slog.Info("postgres worker reaper: removed stale rows",
			"count", n, "cutoff", cutoff)
	}
}

var _ WorkerMap = (*PostgresWorkerMap)(nil)
