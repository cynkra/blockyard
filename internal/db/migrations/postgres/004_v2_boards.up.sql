-- Board storage: tables, RLS policies, PostgREST roles, triggers.
-- PostgreSQL only — SQLite deployments skip this migration.

-- PostgREST roles
DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'blockr_user') THEN
        CREATE ROLE blockr_user NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'anon') THEN
        CREATE ROLE anon NOLOGIN;
    END IF;
END $$;

GRANT USAGE ON SCHEMA public TO blockr_user;

-- Board identity and metadata
CREATE TABLE boards (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_sub   TEXT NOT NULL,
    board_id    TEXT NOT NULL,
    acl_type    TEXT NOT NULL DEFAULT 'private'
                CHECK (acl_type IN ('private', 'public', 'restricted')),
    tags        TEXT[] DEFAULT '{}',
    created_at  TIMESTAMPTZ DEFAULT now(),
    updated_at  TIMESTAMPTZ DEFAULT now(),
    UNIQUE (owner_sub, board_id)
);

-- Versioned board data
CREATE TABLE board_versions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_sub   TEXT NOT NULL,
    board_id    TEXT NOT NULL,
    data        JSONB NOT NULL,
    metadata    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ DEFAULT now(),
    FOREIGN KEY (owner_sub, board_id)
        REFERENCES boards(owner_sub, board_id) ON DELETE CASCADE
);

CREATE INDEX idx_board_versions_lookup
    ON board_versions(owner_sub, board_id, created_at DESC);

-- Sharing (for restricted ACL)
CREATE TABLE board_shares (
    owner_sub       TEXT NOT NULL,
    board_id        TEXT NOT NULL,
    shared_with_sub TEXT NOT NULL,
    created_at      TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (owner_sub, board_id, shared_with_sub),
    FOREIGN KEY (owner_sub, board_id)
        REFERENCES boards(owner_sub, board_id) ON DELETE CASCADE
);

-- Identity helper for RLS.
-- Reads keycloak_sub (custom claim) because vault's Identity OIDC
-- provider hardcodes the standard sub to the vault entity ID. The
-- keycloak_sub claim carries the original IdP subject.
CREATE FUNCTION current_sub() RETURNS TEXT AS $$
    SELECT current_setting('request.jwt.claims', true)::json->>'keycloak_sub'
$$ LANGUAGE sql STABLE;

-- RLS: boards
ALTER TABLE boards ENABLE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON boards
    USING (owner_sub = current_sub())
    WITH CHECK (owner_sub = current_sub());

CREATE POLICY public_read ON boards FOR SELECT
    USING (acl_type = 'public');

CREATE POLICY restricted_read ON boards FOR SELECT
    USING (acl_type = 'restricted' AND EXISTS (
        SELECT 1 FROM board_shares
        WHERE board_shares.owner_sub = boards.owner_sub
        AND board_shares.board_id = boards.board_id
        AND board_shares.shared_with_sub = current_sub()
    ));

-- RLS: board_versions (inherits access from parent board)
ALTER TABLE board_versions ENABLE ROW LEVEL SECURITY;

CREATE POLICY version_owner ON board_versions
    USING (owner_sub = current_sub())
    WITH CHECK (owner_sub = current_sub());

CREATE POLICY version_public ON board_versions FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM boards
        WHERE boards.owner_sub = board_versions.owner_sub
        AND boards.board_id = board_versions.board_id
        AND boards.acl_type = 'public'
    ));

CREATE POLICY version_restricted ON board_versions FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM boards b
        JOIN board_shares bs
            ON b.owner_sub = bs.owner_sub AND b.board_id = bs.board_id
        WHERE b.owner_sub = board_versions.owner_sub
        AND b.board_id = board_versions.board_id
        AND b.acl_type = 'restricted'
        AND bs.shared_with_sub = current_sub()
    ));

-- RLS: board_shares
ALTER TABLE board_shares ENABLE ROW LEVEL SECURITY;

CREATE POLICY shares_owner ON board_shares
    USING (owner_sub = current_sub())
    WITH CHECK (owner_sub = current_sub());

CREATE POLICY shares_see_own ON board_shares FOR SELECT
    USING (shared_with_sub = current_sub());

-- Auto-update boards.updated_at on row modification
CREATE FUNCTION update_boards_timestamp() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER boards_updated_at
    BEFORE UPDATE ON boards
    FOR EACH ROW EXECUTE FUNCTION update_boards_timestamp();

-- Auto-update boards.updated_at when a new version is inserted.
-- The trigger runs as the table owner, but the RLS policy on boards
-- still evaluates current_sub() — which matches, because the user
-- who can insert a version necessarily owns the board.
CREATE FUNCTION update_board_on_version() RETURNS TRIGGER AS $$
BEGIN
    UPDATE boards SET updated_at = now()
    WHERE owner_sub = NEW.owner_sub AND board_id = NEW.board_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER version_updates_board
    AFTER INSERT ON board_versions
    FOR EACH ROW EXECUTE FUNCTION update_board_on_version();

-- Grant PostgREST access
GRANT SELECT, INSERT, UPDATE, DELETE ON boards, board_versions, board_shares
    TO blockr_user;

-- User discovery: expose the existing users table (populated by
-- blockyard's OIDC login flow) so PostgREST can serve user search
-- queries for the sharing UI. No new table needed.
GRANT SELECT ON users TO blockr_user;
