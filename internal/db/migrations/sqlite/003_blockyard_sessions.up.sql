-- phase: expand
--
-- Mirror of the Postgres blockyard_sessions table (see #286). The
-- Postgres-primary session store is only wired up when [redis] +
-- database.driver = "postgres"; SQLite deployments fall back to
-- MemoryStore. The table is created here so the migration numbering
-- stays in lockstep across dialects and so future dialect-agnostic
-- stores have somewhere to land.
CREATE TABLE blockyard_sessions (
    id          TEXT PRIMARY KEY,
    worker_id   TEXT NOT NULL,
    user_sub    TEXT NOT NULL DEFAULT '',
    last_access TEXT NOT NULL,
    expires_at  TEXT NOT NULL
);

CREATE INDEX idx_blockyard_sessions_expires_at  ON blockyard_sessions(expires_at);
CREATE INDEX idx_blockyard_sessions_worker_id   ON blockyard_sessions(worker_id);
CREATE INDEX idx_blockyard_sessions_last_access ON blockyard_sessions(last_access);
