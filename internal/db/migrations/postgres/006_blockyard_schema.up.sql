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

-- Identity stub for RLS. Returns NULL until #284 wires the real
-- users.pg_role resolution — policies below are fail-closed in the
-- interim. Co-located with the new schema; the legacy public
-- counterpart is dropped by 007.
CREATE FUNCTION blockyard.current_sub() RETURNS TEXT AS $$
    SELECT NULL::text
$$ LANGUAGE sql STABLE;

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

CREATE POLICY shares_owner ON blockyard.board_shares
    USING (EXISTS (
        SELECT 1 FROM blockyard.boards
        WHERE boards.id = board_shares.board_ref
        AND boards.owner_sub = blockyard.current_sub()
    ))
    WITH CHECK (EXISTS (
        SELECT 1 FROM blockyard.boards
        WHERE boards.id = board_shares.board_ref
        AND boards.owner_sub = blockyard.current_sub()
    ));

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

CREATE TRIGGER board_versions_prevent_last_delete
BEFORE DELETE ON blockyard.board_versions
FOR EACH ROW EXECUTE FUNCTION blockyard.prevent_last_version_delete();

-- blockr_user grants for the new tables. GRANT USAGE on blockyard
-- already covers schema-level visibility; these add object-level
-- privileges.
GRANT SELECT, INSERT, UPDATE, DELETE
    ON blockyard.boards, blockyard.board_versions, blockyard.board_shares
    TO blockr_user;
GRANT SELECT ON blockyard.users TO blockr_user;
