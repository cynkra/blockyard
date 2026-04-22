package session

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"
)

// PostgresStore implements session.Store against the blockyard_sessions
// table, making Postgres the source of truth for sticky sessions
// (see #286, parent #262).
//
// expires_at is populated from idleTTL on every Set/Touch. A background
// sweep (RunExpiry) deletes rows whose expires_at has passed. This
// replaces Redis-native TTL expiry.
type PostgresStore struct {
	db      *sqlx.DB
	idleTTL time.Duration
}

// NewPostgresStore returns a store backed by the blockyard_sessions table.
// idleTTL drives the expires_at column; 0 means "never expire" and is
// written as a far-future timestamp so the sweep column keeps a sensible
// default without needing NULL handling.
func NewPostgresStore(db *sqlx.DB, idleTTL time.Duration) *PostgresStore {
	return &PostgresStore{db: db, idleTTL: idleTTL}
}

func (s *PostgresStore) expiresAt(from time.Time) time.Time {
	if s.idleTTL <= 0 {
		return from.Add(100 * 365 * 24 * time.Hour)
	}
	return from.Add(s.idleTTL)
}

func (s *PostgresStore) Get(sessionID string) (Entry, bool) {
	ctx := context.Background()
	var e Entry
	err := s.db.QueryRowxContext(ctx,
		`SELECT worker_id, user_sub, last_access
		 FROM blockyard_sessions WHERE id = $1`,
		sessionID,
	).Scan(&e.WorkerID, &e.UserSub, &e.LastAccess)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false
	}
	if err != nil {
		slog.Error("postgres session get", "session_id", sessionID, "error", err)
		return Entry{}, false
	}
	return e, true
}

func (s *PostgresStore) Set(sessionID string, entry Entry) {
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO blockyard_sessions (id, worker_id, user_sub, last_access, expires_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (id) DO UPDATE SET
		     worker_id   = EXCLUDED.worker_id,
		     user_sub    = EXCLUDED.user_sub,
		     last_access = EXCLUDED.last_access,
		     expires_at  = EXCLUDED.expires_at`,
		sessionID, entry.WorkerID, entry.UserSub,
		entry.LastAccess, s.expiresAt(entry.LastAccess),
	)
	if err != nil {
		slog.Error("postgres session set", "session_id", sessionID, "error", err)
	}
}

func (s *PostgresStore) Touch(sessionID string) bool {
	ctx := context.Background()
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE blockyard_sessions
		 SET last_access = $2, expires_at = $3
		 WHERE id = $1`,
		sessionID, now, s.expiresAt(now),
	)
	if err != nil {
		slog.Error("postgres session touch", "session_id", sessionID, "error", err)
		return false
	}
	n, err := res.RowsAffected()
	if err != nil {
		slog.Error("postgres session touch rows", "session_id", sessionID, "error", err)
		return false
	}
	return n > 0
}

func (s *PostgresStore) Delete(sessionID string) {
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM blockyard_sessions WHERE id = $1`, sessionID,
	); err != nil {
		slog.Error("postgres session delete", "session_id", sessionID, "error", err)
	}
}

func (s *PostgresStore) DeleteByWorker(workerID string) int {
	ctx := context.Background()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM blockyard_sessions WHERE worker_id = $1`, workerID,
	)
	if err != nil {
		slog.Error("postgres session delete by worker",
			"worker_id", workerID, "error", err)
		return 0
	}
	n, err := res.RowsAffected()
	if err != nil {
		slog.Error("postgres session delete by worker rows",
			"worker_id", workerID, "error", err)
		return 0
	}
	return int(n)
}

func (s *PostgresStore) CountForWorker(workerID string) int {
	ctx := context.Background()
	var n int
	if err := s.db.QueryRowxContext(ctx,
		`SELECT COUNT(*) FROM blockyard_sessions WHERE worker_id = $1`,
		workerID,
	).Scan(&n); err != nil {
		slog.Error("postgres session count for worker",
			"worker_id", workerID, "error", err)
		return 0
	}
	return n
}

func (s *PostgresStore) CountForWorkers(workerIDs []string) int {
	if len(workerIDs) == 0 {
		return 0
	}
	ctx := context.Background()
	query, args, err := sqlx.In(
		`SELECT COUNT(*) FROM blockyard_sessions WHERE worker_id IN (?)`,
		workerIDs,
	)
	if err != nil {
		slog.Error("postgres session count for workers build", "error", err)
		return 0
	}
	query = s.db.Rebind(query)
	var n int
	if err := s.db.QueryRowxContext(ctx, query, args...).Scan(&n); err != nil {
		slog.Error("postgres session count for workers", "error", err)
		return 0
	}
	return n
}

func (s *PostgresStore) RerouteWorker(oldWorkerID, newWorkerID string) int {
	ctx := context.Background()
	res, err := s.db.ExecContext(ctx,
		`UPDATE blockyard_sessions SET worker_id = $2 WHERE worker_id = $1`,
		oldWorkerID, newWorkerID,
	)
	if err != nil {
		slog.Error("postgres session reroute worker",
			"old_worker_id", oldWorkerID, "new_worker_id", newWorkerID,
			"error", err)
		return 0
	}
	n, err := res.RowsAffected()
	if err != nil {
		slog.Error("postgres session reroute worker rows", "error", err)
		return 0
	}
	return int(n)
}

func (s *PostgresStore) EntriesForWorker(workerID string) map[string]Entry {
	ctx := context.Background()
	result := make(map[string]Entry)
	rows, err := s.db.QueryxContext(ctx,
		`SELECT id, worker_id, user_sub, last_access
		 FROM blockyard_sessions WHERE worker_id = $1`,
		workerID,
	)
	if err != nil {
		slog.Error("postgres session entries for worker",
			"worker_id", workerID, "error", err)
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var e Entry
		if err := rows.Scan(&id, &e.WorkerID, &e.UserSub, &e.LastAccess); err != nil {
			slog.Error("postgres session entries scan",
				"worker_id", workerID, "error", err)
			continue
		}
		result[id] = e
	}
	return result
}

// SweepIdle deletes sessions whose last_access is older than maxAge.
// Matches MemoryStore semantics — callers drive idle sweeps via the
// autoscaler. Natural expiry (expires_at) is handled separately by
// RunExpiry so the two loops can run at different cadences.
func (s *PostgresStore) SweepIdle(maxAge time.Duration) int {
	ctx := context.Background()
	cutoff := time.Now().Add(-maxAge)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM blockyard_sessions WHERE last_access < $1`, cutoff,
	)
	if err != nil {
		slog.Error("postgres session sweep idle", "error", err)
		return 0
	}
	n, err := res.RowsAffected()
	if err != nil {
		slog.Error("postgres session sweep idle rows", "error", err)
		return 0
	}
	return int(n)
}

// RunExpiry deletes rows whose expires_at has passed, every interval.
// Blocks until ctx is cancelled. Caller runs it in a goroutine.
//
// This replaces Redis-native TTL expiry: without it, sessions that are
// created and then never touched again (e.g. a user closes their tab
// mid-handshake) would live forever. SweepIdle is not a substitute —
// that runs only when the operator configures session_idle_ttl.
func (s *PostgresStore) RunExpiry(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepExpired(ctx)
		}
	}
}

func (s *PostgresStore) sweepExpired(ctx context.Context) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM blockyard_sessions WHERE expires_at < now()`)
	if err != nil {
		slog.Error("postgres session expiry sweep", "error", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		slog.Debug("postgres session expiry sweep", "count", n)
	}
}

var _ Store = (*PostgresStore)(nil)
