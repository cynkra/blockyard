CREATE TABLE IF NOT EXISTS apps (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    status                  TEXT NOT NULL DEFAULT 'stopped',
    active_bundle           TEXT REFERENCES bundles(id),
    max_workers_per_app     INTEGER,
    max_sessions_per_worker INTEGER NOT NULL DEFAULT 1,
    memory_limit            TEXT,
    cpu_limit               REAL,
    title                   TEXT,
    description             TEXT,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS bundles (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL DEFAULT 'pending',
    path        TEXT NOT NULL,
    uploaded_at TEXT NOT NULL
);

CREATE INDEX idx_bundles_app_id ON bundles(app_id);

CREATE TABLE IF NOT EXISTS tags (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS app_tags (
    app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    tag_id TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (app_id, tag_id)
);
