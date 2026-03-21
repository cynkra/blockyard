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
