-- Seeds vault_db_admin: the PG identity vault's DB secrets engine
-- uses to administer per-user passwords (password rotation for
-- user_<entity-id> roles provisioned by blockyard at first login).
-- In a real deployment, operators create this role out-of-band via
-- their provisioning tooling; this file automates that for the
-- compose stack.
--
-- Other roles are NOT seeded here:
--   - blockyard_admin   — created by blockyard's startup SQL when
--                         database.board_storage = true (see
--                         internal/boardstorage/admin.go).
--   - blockr_user       — created by blockyard's migration 001.
--   - user_<entity-id>  — created by blockyard's first-login
--                         provisioning, one per user.

CREATE ROLE vault_db_admin WITH LOGIN PASSWORD 'dev-password'
    CREATEROLE NOINHERIT;
