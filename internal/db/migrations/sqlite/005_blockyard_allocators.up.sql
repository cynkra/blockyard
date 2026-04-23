-- phase: expand
--
-- Mirror of the Postgres blockyard_ports / blockyard_uids tables (see
-- #288). The Postgres-primary allocators are only wired up when [redis]
-- + database.driver = "postgres"; SQLite deployments keep the in-memory
-- allocators. The tables are created here so migration numbering stays
-- in lockstep across dialects.
CREATE TABLE blockyard_ports (
    port  INTEGER PRIMARY KEY,
    owner TEXT    NOT NULL
);

CREATE TABLE blockyard_uids (
    uid   INTEGER PRIMARY KEY,
    owner TEXT    NOT NULL
);
