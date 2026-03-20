DROP INDEX IF EXISTS idx_apps_name_live;
ALTER TABLE apps ADD CONSTRAINT apps_name_key UNIQUE (name);
ALTER TABLE apps DROP COLUMN deleted_at;
