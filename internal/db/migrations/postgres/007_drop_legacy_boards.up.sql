-- phase: contract
-- contracts: 001
--
-- Drop the legacy public-schema board-storage objects created by
-- 001_initial: empty in every pre-1.0 deployment (no runtime ever
-- populated them) and replaced by the blockyard-schema versions
-- introduced in 006.
--
-- Also drop the `anon` role (created by 001 for PostgREST). The
-- hello-postgrest example is replaced by hello-postgres in #285, so
-- nothing still needs it. DROP ROLE IF EXISTS is idempotent against
-- both "anon exists from 001" and "anon was never created"
-- deployments. Edited in place rather than added in a new migration
-- because 007 is not released yet (see migrations/released.txt).
--
-- This migration is explicitly named in released.txt-controlled
-- contract references (001 v0.0.3). Rolling back past 006 via
-- MigrateDown also needs 007 to be reversed first; see the
-- irreversible marker in 007.down.sql for the rationale.

-- The three DROPs below trip Atlas's DS102 destructive-change check.
-- The tables are empty in every deployment (the runtime never wrote
-- to them), so the warning is a false positive in this context; the
-- data-loss risk it guards against is what migration 006 already
-- handled by recreating the tables in their new shape under the
-- blockyard schema.
-- atlas:nolint DS102
DROP TABLE IF EXISTS public.board_shares CASCADE;
-- atlas:nolint DS102
DROP TABLE IF EXISTS public.board_versions CASCADE;
-- atlas:nolint DS102
DROP TABLE IF EXISTS public.boards CASCADE;

DROP FUNCTION IF EXISTS public.current_sub();

-- atlas:nolint DS101
DROP ROLE IF EXISTS anon;
