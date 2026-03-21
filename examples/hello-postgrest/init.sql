-- PostgREST database roles.
-- Created at database init time so PostgREST can start immediately.
-- Migration 004 also creates these with IF NOT EXISTS — no conflict.

-- blockr_user: the role PostgREST switches to for authenticated requests.
DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'blockr_user') THEN
        CREATE ROLE blockr_user NOLOGIN;
    END IF;
END $$;

-- anon: the role PostgREST uses for unauthenticated requests.
DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'anon') THEN
        CREATE ROLE anon NOLOGIN;
    END IF;
END $$;

-- authenticator: the role PostgREST connects as.
-- It switches to blockr_user or anon per-request based on the JWT.
CREATE ROLE authenticator LOGIN PASSWORD 'dev-password';
GRANT blockr_user TO authenticator;
GRANT anon TO authenticator;
