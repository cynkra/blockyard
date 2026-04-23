-- phase: contract
-- contracts: 001
--
-- Drop the legacy public-schema board-storage objects created by
-- 001_initial: empty in every pre-1.0 deployment (no runtime ever
-- populated them) and replaced by the blockyard-schema versions
-- introduced in 006.
--
-- The `anon` role (also introduced by 001 for PostgREST) is NOT
-- dropped here. The hello-postgrest example's init.sql still creates
-- and depends on it, and that example is being rewritten in #285 —
-- removing anon now breaks the example's merge-group test for no
-- operational benefit. #285 drops the role when it removes the
-- example.
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
