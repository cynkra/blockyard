-- Revert 006: drop the new board-storage objects, move core tables
-- back to public, restore blockr_user's public-schema grant, and
-- drop the now-empty blockyard schema. Leaves the DB in the state
-- 007.down produced — legacy public.boards/board_versions/
-- board_shares do NOT exist here because 007.down is a no-op
-- (marked irreversible). Running 001.down after this completes the
-- teardown back to a blank DB.

DROP TRIGGER IF EXISTS board_versions_prevent_last_delete
    ON blockyard.board_versions;
DROP FUNCTION IF EXISTS blockyard.prevent_last_version_delete();

REVOKE SELECT, INSERT, UPDATE, DELETE
    ON blockyard.boards, blockyard.board_versions, blockyard.board_shares
    FROM blockr_user;
REVOKE SELECT ON blockyard.users FROM blockr_user;

DROP TABLE IF EXISTS blockyard.board_shares CASCADE;
DROP TABLE IF EXISTS blockyard.board_versions CASCADE;
DROP TABLE IF EXISTS blockyard.boards CASCADE;
DROP FUNCTION IF EXISTS blockyard.current_user_owns_board(UUID);
DROP FUNCTION IF EXISTS blockyard.current_sub();

DROP INDEX IF EXISTS blockyard.idx_users_pg_role;
ALTER TABLE blockyard.users DROP COLUMN IF EXISTS pg_role;

ALTER TABLE blockyard.apps                   SET SCHEMA public;
ALTER TABLE blockyard.bundles                SET SCHEMA public;
ALTER TABLE blockyard.app_access             SET SCHEMA public;
ALTER TABLE blockyard.users                  SET SCHEMA public;
ALTER TABLE blockyard.personal_access_tokens SET SCHEMA public;
ALTER TABLE blockyard.tags                   SET SCHEMA public;
ALTER TABLE blockyard.app_tags               SET SCHEMA public;
ALTER TABLE blockyard.sessions               SET SCHEMA public;
ALTER TABLE blockyard.app_aliases            SET SCHEMA public;
ALTER TABLE blockyard.bundle_logs            SET SCHEMA public;
ALTER TABLE blockyard.app_data_mounts        SET SCHEMA public;
ALTER TABLE blockyard.blockyard_sessions     SET SCHEMA public;
ALTER TABLE blockyard.blockyard_workers      SET SCHEMA public;
ALTER TABLE blockyard.blockyard_ports        SET SCHEMA public;
ALTER TABLE blockyard.blockyard_uids         SET SCHEMA public;

REVOKE USAGE ON SCHEMA blockyard FROM blockr_user;
GRANT USAGE ON SCHEMA public TO blockr_user;

DROP SCHEMA blockyard;
