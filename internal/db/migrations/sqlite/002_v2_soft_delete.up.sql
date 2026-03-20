-- Disable FK checks so we can rebuild the apps table.
PRAGMA foreign_keys = OFF;

-- Rebuild apps table: replace column-level UNIQUE on name with a
-- partial unique index that only covers live (non-deleted) apps.
CREATE TABLE apps_new (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL,
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
    updated_at              TEXT NOT NULL,
    deleted_at              TEXT
);

INSERT INTO apps_new
    SELECT id, name, owner, access_type, active_bundle,
           max_workers_per_app, max_sessions_per_worker,
           memory_limit, cpu_limit, title, description,
           created_at, updated_at, NULL
    FROM apps;

DROP TABLE apps;
ALTER TABLE apps_new RENAME TO apps;

CREATE UNIQUE INDEX idx_apps_name_live ON apps(name) WHERE deleted_at IS NULL;

PRAGMA foreign_keys = ON;
