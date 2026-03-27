-- SQLite lacks DROP COLUMN before 3.35; recreate tables to remove columns.
-- In practice, down migrations are only used in dev, and the temp-file
-- SQLite driver supports ALTER TABLE DROP COLUMN.

-- Remove enabled column from apps
-- (SQLite 3.35+ supports ALTER TABLE DROP COLUMN for simple cases)
ALTER TABLE apps DROP COLUMN enabled;

-- Remove bundle deployment columns
ALTER TABLE bundles DROP COLUMN pinned;
ALTER TABLE bundles DROP COLUMN deployed_at;
ALTER TABLE bundles DROP COLUMN deployed_by;

-- Drop sessions table
DROP INDEX IF EXISTS idx_sessions_status;
DROP INDEX IF EXISTS idx_sessions_worker;
DROP INDEX IF EXISTS idx_sessions_user;
DROP INDEX IF EXISTS idx_sessions_app_started;
DROP TABLE IF EXISTS sessions;
