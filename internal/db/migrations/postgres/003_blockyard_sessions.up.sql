-- phase: expand
--
-- Postgres-primary session store (see #286, parent #262).
-- Distinct from the existing `sessions` audit table — that one records
-- session history for metrics; this one is the proxy's sticky-session
-- source of truth, inverting the previous Redis-primary model.
--
-- id is TEXT (not UUID) to match MemoryStore/RedisStore semantics: all
-- stores accept any string session ID, so a malformed cookie returns
-- "not found" rather than a type error.
CREATE TABLE blockyard_sessions (
    id          TEXT PRIMARY KEY,
    worker_id   TEXT NOT NULL,
    user_sub    TEXT NOT NULL DEFAULT '',
    last_access TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_blockyard_sessions_expires_at  ON blockyard_sessions(expires_at);
CREATE INDEX idx_blockyard_sessions_worker_id   ON blockyard_sessions(worker_id);
-- last_access index: SweepIdle becomes primary eviction path when the
-- operator configures session_idle_ttl shorter than expires_at.
CREATE INDEX idx_blockyard_sessions_last_access ON blockyard_sessions(last_access);
