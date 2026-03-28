-- Sessions table: tracks user -> app -> worker -> session chain
CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    worker_id   TEXT NOT NULL,
    user_sub    TEXT,
    started_at  TEXT NOT NULL,
    ended_at    TEXT,
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'ended', 'crashed'))
);

CREATE INDEX idx_sessions_app_started ON sessions(app_id, started_at DESC);
CREATE INDEX idx_sessions_user ON sessions(user_sub, app_id, started_at DESC);
CREATE INDEX idx_sessions_worker ON sessions(worker_id, started_at DESC);
CREATE INDEX idx_sessions_status ON sessions(status);

-- Bundle deployment tracking
ALTER TABLE bundles ADD COLUMN deployed_by TEXT;
ALTER TABLE bundles ADD COLUMN deployed_at TEXT;
ALTER TABLE bundles ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;

-- Backfill: existing ready bundles get deployed_at = uploaded_at, deployed_by = app owner.
UPDATE bundles SET
    deployed_at = uploaded_at,
    deployed_by = (SELECT owner FROM apps WHERE apps.id = bundles.app_id)
WHERE status = 'ready';

-- App enable/disable
ALTER TABLE apps ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1;
