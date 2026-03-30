CREATE TABLE bundle_logs (
    bundle_id   TEXT PRIMARY KEY REFERENCES bundles(id) ON DELETE CASCADE,
    output      TEXT NOT NULL,
    created_at  TEXT NOT NULL
);
