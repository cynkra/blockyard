-- phase: expand
--
-- Relocate blockyard-owned objects into a dedicated `blockyard`
-- schema (see #283) and introduce the finalized board-storage data
-- model. Core tables (apps, bundles, users, …) keep their data via
-- ALTER TABLE … SET SCHEMA. The legacy public.boards/board_versions/
-- board_shares tables created by 001 stay in place here — they are
-- empty in every pre-1.0 deployment (the runtime side of board
-- storage lands in #284/#285) — and are removed by the paired
-- contract migration 007.
--
-- Only PG13+ syntax. The Go caller sets search_path to
-- `blockyard, public`; before CREATE SCHEMA runs below the path
-- resolves through public (PG silently skips missing schemas in the
-- path), so unqualified references from earlier migrations continue
-- to find their legacy homes. After CREATE SCHEMA, `blockyard`
-- resolves first.

CREATE SCHEMA IF NOT EXISTS blockyard;

-- Swap blockr_user's usage grant over to the new schema. 001 granted
-- public; the new home is blockyard. Leaving the public grant would
-- keep giving the role visibility into objects that no longer belong
-- to the application.
REVOKE USAGE ON SCHEMA public FROM blockr_user;
GRANT USAGE ON SCHEMA blockyard TO blockr_user;

-- Move core tables. SET SCHEMA rewrites pg_class; rows and
-- dependencies (indexes, FKs, CHECKs, privilege grants) follow.
ALTER TABLE public.apps                   SET SCHEMA blockyard;
ALTER TABLE public.bundles                SET SCHEMA blockyard;
ALTER TABLE public.app_access             SET SCHEMA blockyard;
ALTER TABLE public.users                  SET SCHEMA blockyard;
ALTER TABLE public.personal_access_tokens SET SCHEMA blockyard;
ALTER TABLE public.tags                   SET SCHEMA blockyard;
ALTER TABLE public.app_tags               SET SCHEMA blockyard;
ALTER TABLE public.sessions               SET SCHEMA blockyard;
ALTER TABLE public.app_aliases            SET SCHEMA blockyard;
ALTER TABLE public.bundle_logs            SET SCHEMA blockyard;
ALTER TABLE public.app_data_mounts        SET SCHEMA blockyard;
ALTER TABLE public.blockyard_sessions     SET SCHEMA blockyard;
ALTER TABLE public.blockyard_workers      SET SCHEMA blockyard;
ALTER TABLE public.blockyard_ports        SET SCHEMA blockyard;
ALTER TABLE public.blockyard_uids         SET SCHEMA blockyard;

-- Per-user PG role mapping. Populated by #284's first-login
-- provisioning; NULL for users who never had board storage enabled.
-- current_sub() below resolves current_user → sub via this column.
-- Partial unique index allows many NULLs but enforces one user per
-- populated role name.
ALTER TABLE blockyard.users ADD COLUMN pg_role TEXT;
CREATE UNIQUE INDEX idx_users_pg_role
    ON blockyard.users(pg_role) WHERE pg_role IS NOT NULL;

-- New board-storage tables in the finalized shape. Created
-- schema-qualified so the rest of the migration is independent of
-- whichever search_path resolution migrate picks.
CREATE TABLE blockyard.boards (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_sub  TEXT NOT NULL REFERENCES blockyard.users(sub) ON DELETE CASCADE,
    board_id   TEXT NOT NULL
               CHECK (board_id ~ '^[a-z0-9][a-z0-9-]*[a-z0-9]$'
                      AND length(board_id) <= 63),
    name       TEXT NOT NULL
               CHECK (length(name) BETWEEN 1 AND 255),
    acl_type   TEXT NOT NULL DEFAULT 'private'
               CHECK (acl_type IN ('private', 'public', 'restricted')),
    tags       TEXT[] NOT NULL DEFAULT '{}',
    metadata   JSONB NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (owner_sub, board_id)
);

CREATE INDEX idx_boards_tags ON blockyard.boards USING GIN (tags);

CREATE TABLE blockyard.board_versions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    board_ref  UUID NOT NULL REFERENCES blockyard.boards(id) ON DELETE CASCADE,
    data       JSONB NOT NULL,
    format     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_board_versions_lookup
    ON blockyard.board_versions(board_ref, created_at DESC);

CREATE TABLE blockyard.board_shares (
    board_ref       UUID NOT NULL REFERENCES blockyard.boards(id) ON DELETE CASCADE,
    shared_with_sub TEXT NOT NULL REFERENCES blockyard.users(sub) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (board_ref, shared_with_sub)
);

CREATE INDEX idx_board_shares_shared_with
    ON blockyard.board_shares(shared_with_sub);

-- Identity resolution for RLS. Maps the PG role that opened this
-- session (session_user, not current_user) back to the OIDC sub
-- via users.pg_role. Using session_user is load-bearing: it keeps
-- the mapping stable across SECURITY DEFINER boundaries — where
-- current_user flips to the function owner — so the same helper
-- works both in invoker-scoped policy bodies and inside the
-- DEFINER-scoped helpers below. NOLOGIN admin connections
-- (blockyard_admin) have no users row and therefore resolve to
-- NULL, which fail-closes the policies they traverse.
CREATE FUNCTION blockyard.current_sub() RETURNS TEXT AS $$
    SELECT sub FROM blockyard.users WHERE pg_role = session_user
$$ LANGUAGE sql STABLE;

-- RLS policies on `boards` reference `board_shares`, and the
-- owner-side policy on `board_shares` would otherwise need to
-- reference `boards` — creating a cross-table policy cycle that PG
-- rejects at query time ("infinite recursion detected in policy").
--
-- SECURITY DEFINER helper breaks the cycle: it reads `boards` as
-- the function owner (migration user), bypassing RLS on that table.
-- The predicate is locked to the caller's own identity via
-- current_sub() (which uses session_user, unchanged across the
-- SECURITY DEFINER boundary), so the helper leaks nothing beyond
-- "do I own this board ID?" — which the caller already learns from
-- their own SELECTs. search_path is pinned to guard against
-- schema-shadow attacks by callers with CREATE on other schemas.
CREATE FUNCTION blockyard.current_user_owns_board(b_id UUID) RETURNS BOOLEAN AS $$
    SELECT EXISTS (
        SELECT 1 FROM blockyard.boards
        WHERE id = b_id AND owner_sub = blockyard.current_sub()
    );
$$ LANGUAGE sql SECURITY DEFINER STABLE SET search_path = blockyard, pg_catalog;

-- RLS policies. Ownership is a board-level fact, so policies on
-- children dereference via EXISTS against blockyard.boards.
ALTER TABLE blockyard.boards ENABLE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON blockyard.boards
    USING (owner_sub = blockyard.current_sub())
    WITH CHECK (owner_sub = blockyard.current_sub());

CREATE POLICY public_read ON blockyard.boards FOR SELECT
    USING (acl_type = 'public');

CREATE POLICY restricted_read ON blockyard.boards FOR SELECT
    USING (acl_type = 'restricted' AND EXISTS (
        SELECT 1 FROM blockyard.board_shares
        WHERE board_shares.board_ref = boards.id
        AND board_shares.shared_with_sub = blockyard.current_sub()
    ));

ALTER TABLE blockyard.board_versions ENABLE ROW LEVEL SECURITY;

CREATE POLICY version_owner ON blockyard.board_versions
    USING (EXISTS (
        SELECT 1 FROM blockyard.boards
        WHERE boards.id = board_versions.board_ref
        AND boards.owner_sub = blockyard.current_sub()
    ))
    WITH CHECK (EXISTS (
        SELECT 1 FROM blockyard.boards
        WHERE boards.id = board_versions.board_ref
        AND boards.owner_sub = blockyard.current_sub()
    ));

CREATE POLICY version_public ON blockyard.board_versions FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM blockyard.boards
        WHERE boards.id = board_versions.board_ref
        AND boards.acl_type = 'public'
    ));

CREATE POLICY version_restricted ON blockyard.board_versions FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM blockyard.boards b
        JOIN blockyard.board_shares bs ON bs.board_ref = b.id
        WHERE b.id = board_versions.board_ref
        AND b.acl_type = 'restricted'
        AND bs.shared_with_sub = blockyard.current_sub()
    ));

ALTER TABLE blockyard.board_shares ENABLE ROW LEVEL SECURITY;

-- Owner-side policy uses the SECURITY DEFINER helper rather than
-- inlining `EXISTS (… FROM blockyard.boards …)` to avoid the policy
-- cycle with boards.restricted_read (which references board_shares).
CREATE POLICY shares_owner ON blockyard.board_shares
    USING (blockyard.current_user_owns_board(board_ref))
    WITH CHECK (blockyard.current_user_owns_board(board_ref));

CREATE POLICY shares_see_own ON blockyard.board_shares FOR SELECT
    USING (shared_with_sub = blockyard.current_sub());

-- Invariant: a board always has >= 1 version. Deleting the last
-- raises restrict_violation — keeps rack_delete (prune one version)
-- and rack_purge (drop the board) semantically distinct.
CREATE FUNCTION blockyard.prevent_last_version_delete() RETURNS TRIGGER AS $$
BEGIN
    IF (SELECT count(*) FROM blockyard.board_versions
        WHERE board_ref = OLD.board_ref) = 1 THEN
        RAISE EXCEPTION
            'cannot delete the last version of board %; purge the board instead',
            OLD.board_ref
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

-- WHEN (pg_trigger_depth() = 0) pins the check to direct DELETEs.
-- Without this clause, cascade deletes from `boards` fire this
-- trigger for every child row; the last surviving version hits
-- count=1 and raises, aborting the whole parent DELETE. Cascade
-- paths run inside PG's RI machinery at depth >= 1 and must bypass
-- the check so rack_purge semantics work; rack_delete (direct prune
-- of a single version) still raises at depth 0 when count=1.
CREATE TRIGGER board_versions_prevent_last_delete
BEFORE DELETE ON blockyard.board_versions
FOR EACH ROW
WHEN (pg_trigger_depth() = 0)
EXECUTE FUNCTION blockyard.prevent_last_version_delete();

-- blockr_user grants for the new tables. GRANT USAGE on blockyard
-- already covers schema-level visibility; these add object-level
-- privileges.
GRANT SELECT, INSERT, UPDATE, DELETE
    ON blockyard.boards, blockyard.board_versions, blockyard.board_shares
    TO blockr_user;
GRANT SELECT ON blockyard.users TO blockr_user;
