ALTER TABLE apps ADD COLUMN deleted_at TEXT;

-- Replace the column-level UNIQUE on name with a partial unique index.
ALTER TABLE apps DROP CONSTRAINT apps_name_key;
CREATE UNIQUE INDEX idx_apps_name_live ON apps(name) WHERE deleted_at IS NULL;
