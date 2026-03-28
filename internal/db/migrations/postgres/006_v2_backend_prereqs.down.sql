ALTER TABLE apps DROP COLUMN enabled;

ALTER TABLE bundles DROP COLUMN pinned;
ALTER TABLE bundles DROP COLUMN deployed_at;
ALTER TABLE bundles DROP COLUMN deployed_by;

DROP INDEX IF EXISTS idx_sessions_status;
DROP INDEX IF EXISTS idx_sessions_worker;
DROP INDEX IF EXISTS idx_sessions_user;
DROP INDEX IF EXISTS idx_sessions_app_started;
DROP TABLE IF EXISTS sessions;
