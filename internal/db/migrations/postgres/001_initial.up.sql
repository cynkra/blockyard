-- phase: expand
CREATE TABLE apps (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL,
    owner                   TEXT NOT NULL DEFAULT 'admin',
    access_type             TEXT NOT NULL DEFAULT 'acl'
                            CHECK (access_type IN ('acl', 'logged_in', 'public')),
    active_bundle           TEXT,
    max_workers_per_app     INTEGER,
    max_sessions_per_worker INTEGER DEFAULT 1,
    memory_limit            TEXT,
    cpu_limit               DOUBLE PRECISION,
    title                   TEXT,
    description             TEXT,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL,
    deleted_at              TEXT,
    pre_warmed_sessions     INTEGER NOT NULL DEFAULT 0,
    refresh_schedule        TEXT NOT NULL DEFAULT '',
    last_refresh_at         TEXT,
    enabled                 INTEGER NOT NULL DEFAULT 1
);

CREATE UNIQUE INDEX idx_apps_name_live ON apps(name) WHERE deleted_at IS NULL;

CREATE TABLE bundles (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL DEFAULT 'pending',
    uploaded_at TEXT NOT NULL,
    deployed_by TEXT,
    deployed_at TEXT,
    pinned      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_bundles_app_id ON bundles(app_id);

ALTER TABLE apps ADD CONSTRAINT fk_apps_active_bundle
    FOREIGN KEY (active_bundle) REFERENCES bundles(id) ON DELETE SET NULL;

CREATE TABLE app_access (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    principal   TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('user')),
    role        TEXT NOT NULL CHECK (role IN ('viewer', 'collaborator')),
    granted_by  TEXT NOT NULL,
    granted_at  TEXT NOT NULL,
    PRIMARY KEY (app_id, principal, kind)
);

CREATE TABLE users (
    sub        TEXT PRIMARY KEY,
    email      TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL DEFAULT '',
    role       TEXT NOT NULL DEFAULT 'viewer',
    active     INTEGER NOT NULL DEFAULT 1,
    last_login TEXT NOT NULL
);

CREATE TABLE personal_access_tokens (
    id           TEXT PRIMARY KEY,
    token_hash   BYTEA NOT NULL UNIQUE,
    user_sub     TEXT NOT NULL REFERENCES users(sub),
    name         TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT,
    last_used_at TEXT,
    revoked      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_pat_token_hash ON personal_access_tokens(token_hash);
CREATE INDEX idx_pat_user_sub ON personal_access_tokens(user_sub);

CREATE TABLE tags (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE app_tags (
    app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    tag_id TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (app_id, tag_id)
);

CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    worker_id   TEXT NOT NULL,
    user_sub    TEXT,
    started_at  TEXT NOT NULL,
    ended_at    TEXT,
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'ended', 'crashed'))
);

CREATE INDEX idx_sessions_app_started ON sessions(app_id, started_at DESC);
CREATE INDEX idx_sessions_user ON sessions(user_sub, app_id, started_at DESC);
CREATE INDEX idx_sessions_worker ON sessions(worker_id, started_at DESC);
CREATE INDEX idx_sessions_status ON sessions(status);

CREATE TABLE app_aliases (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    name        TEXT NOT NULL UNIQUE,
    phase       TEXT NOT NULL CHECK (phase IN ('alias', 'redirect')),
    expires_at  TEXT NOT NULL
);

CREATE INDEX idx_app_aliases_app_id ON app_aliases(app_id);

CREATE TABLE bundle_logs (
    bundle_id   TEXT PRIMARY KEY REFERENCES bundles(id) ON DELETE CASCADE,
    output      TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

-- Board storage: PostgreSQL only.
-- Boards use native TIMESTAMPTZ (not TEXT) because they are never
-- shared with SQLite and benefit from timezone-aware comparison.

DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'blockr_user') THEN
        CREATE ROLE blockr_user NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'anon') THEN
        CREATE ROLE anon NOLOGIN;
    END IF;
END $$;

GRANT USAGE ON SCHEMA public TO blockr_user;

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
CREATE FUNCTION current_sub() RETURNS TEXT AS $$
    SELECT current_setting('request.jwt.claims', true)::json->>'idp_sub'
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

-- RLS: board_versions
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

-- Auto-update boards.updated_at on row modification.
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

GRANT SELECT, INSERT, UPDATE, DELETE ON boards, board_versions, board_shares
    TO blockr_user;
GRANT SELECT ON users TO blockr_user;
