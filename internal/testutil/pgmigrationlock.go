package testutil

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgMigrationLockKey is an arbitrary int64 used as the cluster-wide
// advisory-lock key that serializes test migration runs on a shared
// PG cluster (#317). Value is opaque — just needs to not collide
// with other advisory locks in the same PG instance.
const pgMigrationLockKey int64 = 317_317_317

// AcquirePGMigrationLock holds a cluster-wide advisory lock on a
// dedicated single-connection pool until the returned fn runs.
// Callers must wrap the migration invocation — db.Open via
// golang-migrate — and any additional CREATE ROLE that targets a
// cluster-wide role (pg_authid is shared across databases). The
// lock serializes these against parallel `go test` packages that
// would otherwise race on the pg_authid unique index (#317).
//
// baseURL is a superuser connection URL (any database works; the
// lock itself is cluster-scoped).
func AcquirePGMigrationLock(t *testing.T, baseURL string) (release func()) {
	t.Helper()
	release, err := acquirePGMigrationLock(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	return release
}

// AcquirePGMigrationLockMain is the TestMain-flavoured variant of
// AcquirePGMigrationLock. Logs to stderr and os.Exits on failure
// because TestMain doesn't have a *testing.T.
func AcquirePGMigrationLockMain(baseURL string) (release func()) {
	release, err := acquirePGMigrationLock(baseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return release
}

func acquirePGMigrationLock(baseURL string) (release func(), err error) {
	locker, err := sql.Open("pgx", baseURL)
	if err != nil {
		return nil, fmt.Errorf("migration lock connect: %w", err)
	}
	locker.SetMaxOpenConns(1)
	if _, err := locker.Exec(`SELECT pg_advisory_lock($1)`, pgMigrationLockKey); err != nil {
		locker.Close()
		return nil, fmt.Errorf("migration lock acquire: %w", err)
	}
	return func() {
		locker.Exec(`SELECT pg_advisory_unlock($1)`, pgMigrationLockKey)
		locker.Close()
	}, nil
}
