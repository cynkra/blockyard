-- SQLite does not support DROP COLUMN before 3.35.0.
-- Recreate the table without the new columns.
CREATE TABLE apps_backup AS SELECT
    id, name, owner, access_type, active_bundle,
    max_workers_per_app, max_sessions_per_worker,
    memory_limit, cpu_limit, title, description,
    created_at, updated_at, deleted_at, pre_warmed_seats
FROM apps;
DROP TABLE apps;
ALTER TABLE apps_backup RENAME TO apps;
