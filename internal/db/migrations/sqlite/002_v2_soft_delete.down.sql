-- Rebuild apps table: restore column-level UNIQUE on name, remove deleted_at.
PRAGMA foreign_keys = OFF;

CREATE TABLE apps_new (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    owner                   TEXT NOT NULL DEFAULT 'admin',
    access_type             TEXT NOT NULL DEFAULT 'acl'
                            CHECK (access_type IN ('acl', 'logged_in', 'public')),
    active_bundle           TEXT REFERENCES bundles(id) ON DELETE SET NULL,
    max_workers_per_app     INTEGER,
    max_sessions_per_worker INTEGER DEFAULT 1,
    memory_limit            TEXT,
    cpu_limit               REAL,
    title                   TEXT,
    description             TEXT,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);

INSERT INTO apps_new
    SELECT id, name, owner, access_type, active_bundle,
           max_workers_per_app, max_sessions_per_worker,
           memory_limit, cpu_limit, title, description,
           created_at, updated_at
    FROM apps
    WHERE deleted_at IS NULL;

DROP INDEX IF EXISTS idx_apps_name_live;
DROP TABLE apps;
ALTER TABLE apps_new RENAME TO apps;

PRAGMA foreign_keys = ON;
