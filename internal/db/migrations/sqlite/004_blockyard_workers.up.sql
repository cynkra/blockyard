-- phase: expand
--
-- Mirror of the Postgres blockyard_workers table (see #287). The
-- Postgres-primary worker stores are only wired up when [redis] +
-- database.driver = "postgres"; SQLite deployments keep the in-memory
-- stores. The table is created here so migration numbering stays in
-- lockstep across dialects and so future dialect-agnostic stores have
-- somewhere to land.
CREATE TABLE blockyard_workers (
    id             TEXT PRIMARY KEY,
    address        TEXT NOT NULL DEFAULT '',
    app_id         TEXT NOT NULL DEFAULT '',
    bundle_id      TEXT NOT NULL DEFAULT '',
    server_id      TEXT NOT NULL DEFAULT '',
    draining       INTEGER NOT NULL DEFAULT 0,
    idle_since     TEXT,
    started_at     TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_heartbeat TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_blockyard_workers_app_id         ON blockyard_workers(app_id);
CREATE INDEX idx_blockyard_workers_server_id      ON blockyard_workers(server_id);
CREATE INDEX idx_blockyard_workers_last_heartbeat ON blockyard_workers(last_heartbeat);
