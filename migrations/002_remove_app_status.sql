-- SQLite doesn't support ALTER TABLE DROP COLUMN before 3.35.0.
-- Use the table-rebuild pattern for broad compatibility.
CREATE TABLE apps_new (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    active_bundle           TEXT REFERENCES bundles(id),
    max_workers_per_app     INTEGER,
    max_sessions_per_worker INTEGER NOT NULL DEFAULT 1,
    memory_limit            TEXT,
    cpu_limit               REAL,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);

INSERT INTO apps_new SELECT
    id, name, active_bundle, max_workers_per_app,
    max_sessions_per_worker, memory_limit, cpu_limit,
    created_at, updated_at
FROM apps;

DROP TABLE apps;
ALTER TABLE apps_new RENAME TO apps;
