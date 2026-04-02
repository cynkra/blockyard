REVOKE SELECT ON users FROM blockr_user;

DROP TRIGGER IF EXISTS version_updates_board ON board_versions;
DROP TRIGGER IF EXISTS boards_updated_at ON boards;
DROP FUNCTION IF EXISTS update_board_on_version();
DROP FUNCTION IF EXISTS update_boards_timestamp();

DROP TABLE IF EXISTS board_shares CASCADE;
DROP TABLE IF EXISTS board_versions CASCADE;
DROP TABLE IF EXISTS boards CASCADE;
DROP FUNCTION IF EXISTS current_sub();

-- Do not drop blockr_user/anon roles — they may be used by other
-- services. Role cleanup is an operator concern.

DROP TABLE IF EXISTS bundle_logs;
DROP TABLE IF EXISTS app_aliases;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS app_tags;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS personal_access_tokens;
DROP TABLE IF EXISTS app_access;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS fk_apps_active_bundle;
DROP TABLE IF EXISTS bundles;
DROP TABLE IF EXISTS apps;
DROP TABLE IF EXISTS users;
