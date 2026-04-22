-- phase: expand
--
-- Postgres-primary worker registry + metadata (see #287, parent #262).
-- Unifies the previously separate Redis stores (`registry:{id}` string
-- and `worker:{id}` hash) into one row-per-worker table. Postgres becomes
-- source of truth; Redis drops to an optional read-through cache so that
-- a Redis restart does not drop workers.
--
-- Defaults on the *-required* columns let PostgresRegistry.Set (which
-- only knows id+address) and PostgresWorkerMap.Set (which only knows the
-- metadata) each do standalone upserts. In production both Set calls
-- happen together during spawn, and the row converges to the full shape;
-- the defaults only cover the "one side ran alone" case (tests, or a
-- future caller that doesn't mirror both).
CREATE TABLE blockyard_workers (
    id             TEXT PRIMARY KEY,
    address        TEXT NOT NULL DEFAULT '',
    app_id         TEXT NOT NULL DEFAULT '',
    bundle_id      TEXT NOT NULL DEFAULT '',
    server_id      TEXT NOT NULL DEFAULT '',
    draining       BOOLEAN NOT NULL DEFAULT false,
    idle_since     TIMESTAMPTZ,
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_blockyard_workers_app_id         ON blockyard_workers(app_id);
CREATE INDEX idx_blockyard_workers_server_id      ON blockyard_workers(server_id);
CREATE INDEX idx_blockyard_workers_last_heartbeat ON blockyard_workers(last_heartbeat);
