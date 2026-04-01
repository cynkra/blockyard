# Migration Authoring Guide

Canonical reference for writing blockyard database migrations.

## The expand-and-contract pattern

Every migration must be backward-compatible with the previous release.
The old server must be able to read and write the database after new
migrations run. This is enforced via a two-phase pattern:

- **Expand** (this release): additive changes only. The old server must
  be able to read and write the database after these run.
- **Contract** (next release): remove deprecated schema. Safe because no
  server running the previous code is still alive.

## Migrations must be self-contained

**This is not mechanically enforced. Read carefully.**

Every migration must carry its own data transformations in SQL. Never
rely on application code running between an expand and its contract to
move, backfill, or transform data. Migrations run sequentially by
number — if a user upgrades from v1.0 to v3.0, the application code
from v2.0 never executes. Any data transformation that lived only in
v2.0's application code is skipped, and the contract migration will
destroy data that was never migrated.

Correct — backfill in the migration:
```sql
-- phase: expand
ALTER TABLE users ADD COLUMN email_normalized TEXT NOT NULL DEFAULT '';
UPDATE users SET email_normalized = lower(email);
```

Wrong — backfill deferred to application code:
```sql
-- phase: expand
ALTER TABLE users ADD COLUMN email_normalized TEXT NOT NULL DEFAULT '';
-- Application code will backfill this over the next few days...
```

If a backfill is too large to run in a single migration, it must still
be expressed as SQL in the migration file (batched `UPDATE` with a loop,
or a temporary trigger that populates on read). The migration may be
slow, but it will be correct regardless of which versions the user
skipped.

## Allowed operations (expand phase)

- `ADD COLUMN` with a `DEFAULT` value (or nullable)
- `CREATE TABLE`
- `CREATE INDEX` (non-unique, or unique on new tables only)
- `CREATE INDEX CONCURRENTLY` (PostgreSQL; avoids table locks)
- `ADD CHECK` constraint with `NOT VALID` (PostgreSQL; deferred
  validation)

## Prohibited operations (without a paired contract in the next release)

- `DROP COLUMN` — old server may SELECT or INSERT it
- `RENAME COLUMN` — old server references the old name
- `ALTER COLUMN ... TYPE` — old server assumes the old type
- `DROP TABLE` — unless created in the same migration batch
- `ALTER TABLE ... ADD ... NOT NULL` without `DEFAULT` — old server
  INSERTs will fail
- `RENAME TABLE` — old server references the old name
- `DROP INDEX` on an index the old server relies on for performance

## Migration file conventions

- Sequential numbering: `NNN_description.up.sql` /
  `NNN_description.down.sql`
- Both up and down files must exist. Down migrations are a production
  path (`by admin rollback`), not just a dev tool. Irreversible
  migrations (e.g., data backfills) must be explicitly marked
  `-- irreversible: <reason>` — this blocks automated rollback past
  that point.
- Both SQLite and PostgreSQL tracks must have matching migration
  numbers. Use `-- no-op: <reason>` for dialect-specific migrations
  that don't apply to the other dialect.
- One logical change per migration number — don't bundle unrelated DDL.
- Comments explaining *why* for non-obvious choices.

## Phase tags

Every `.up.sql` file must begin with a phase tag on its first line:

```sql
-- phase: expand
```

or

```sql
-- phase: contract
-- contracts: 002
```

Rules enforced by the convention check:

1. Every `.up.sql` has exactly one `-- phase:` tag (first line), with
   value `expand` or `contract`.
2. Every `contract` migration has a `-- contracts: NNN` line referencing
   one or more expand migration numbers (comma-separated for
   multi-expand contracts, e.g. `-- contracts: 002, 005`).
3. Referenced migration numbers must exist and be lower than the current
   migration number.
4. Referenced migrations must themselves be tagged `expand` (you don't
   contract a contract).
5. Referenced expand migrations must appear in `released.txt` — i.e.,
   they must have shipped in a prior release.

Phase tags are only required on `.up.sql` files — the `.down.sql`
inherits the phase from its `.up.sql` pair.

A release can contain any mix of expands and contracts.

## Contract phase procedure

- The release notes for the expand phase document what will be
  contracted in the next release.
- The contract migration references the expand migration number via its
  `-- contracts: NNN` tag:
  ```sql
  -- phase: contract
  -- contracts: 002
  ```
- The convention check verifies that every referenced expand appears in
  `released.txt`. If the expand hasn't shipped yet, the check fails —
  no judgment call required.
- The release process appends newly-shipped migration numbers to
  `released.txt` when tagging a release.
