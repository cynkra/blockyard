package boardstorage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/google/uuid"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgBaseURL is the superuser connection URL, populated by the CI job
// that runs this package's tests. Empty locally → tests skip.
var pgBaseURL = os.Getenv("BLOCKYARD_TEST_POSTGRES_URL")

// boardStoragePgDB opens an isolated PostgreSQL database for one test,
// runs migrations, and pre-creates roles the board-storage flow
// depends on but that migrations don't provide (they're deployment
// setup in production — see #285):
//
//   - vault_db_admin: GRANT target for per-user ADMIN OPTION chain.
//   - blockyard_admin: created here so the provisioner's GRANT
//     blockr_user path runs exactly once; tests also assert the
//     bootstrap SQL is idempotent via EnsureBlockyardAdmin.
//
// Each test gets a fresh database via CREATE DATABASE on the shared
// server; the database is dropped on cleanup.
func boardStoragePgDB(t *testing.T) *db.DB {
	t.Helper()
	if pgBaseURL == "" {
		t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set")
	}
	dbName := "test_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
	admin, err := sql.Open("pgx", pgBaseURL)
	if err != nil {
		t.Fatalf("admin open: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	admin.Close()

	testURL := replaceDBName(pgBaseURL, dbName)
	d, err := db.Open(config.DatabaseConfig{Driver: "postgres", URL: testURL})
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}

	// Pre-create roles that operators set up in deployment (#285).
	// Tests don't run the full deployment; creating them here
	// satisfies the per-user GRANT chain the provisioner emits.
	mustExec(t, d, `DO $$ BEGIN
        IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'vault_db_admin') THEN
            CREATE ROLE vault_db_admin NOINHERIT;
        END IF;
    END $$`)

	t.Cleanup(func() {
		d.Close()
		cleanup, _ := sql.Open("pgx", pgBaseURL)
		defer cleanup.Close()
		// Kill lingering connections so DROP DATABASE succeeds even
		// if pool close left a conn in TIME_WAIT.
		cleanup.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
                        WHERE datname = $1`, dbName)
		cleanup.Exec("DROP DATABASE IF EXISTS " + dbName)
	})
	return d
}

func mustExec(t *testing.T, d *db.DB, stmt string, args ...any) {
	t.Helper()
	if _, err := d.ExecContext(context.Background(), stmt, args...); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

func replaceDBName(rawURL, newDB string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.Path = "/" + newDB
	return u.String()
}

// connectAs returns a *sql.DB authenticated as roleName with the
// given password, search_path pinned to blockyard+public for RLS
// tests. Caller closes.
func connectAs(t *testing.T, dbName, roleName, password string) *sql.DB {
	t.Helper()
	conn := openAs(dbName, roleName, password)
	if conn == nil {
		t.Fatalf("connect as %s failed", roleName)
	}
	return conn
}

// tryConnectAs returns nil when the role cannot log in (e.g. after
// NOLOGIN) without failing the test. Caller closes a non-nil result.
func tryConnectAs(dbName, roleName, password string) *sql.DB {
	return openAs(dbName, roleName, password)
}

func openAs(dbName, roleName, password string) *sql.DB {
	u, err := url.Parse(pgBaseURL)
	if err != nil {
		return nil
	}
	u.User = url.UserPassword(roleName, password)
	u.Path = "/" + dbName
	q := u.Query()
	q.Set("search_path", "blockyard,public")
	u.RawQuery = q.Encode()
	conn, err := sql.Open("pgx", u.String())
	if err != nil {
		return nil
	}
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil
	}
	return conn
}

// dbNameFromURL extracts the path-as-db-name from a connection URL,
// used to thread the test database name into per-role connect-as
// helpers.
func dbNameFromURL(t *testing.T, d *db.DB) string {
	t.Helper()
	// The sqlx.DB doesn't expose its URL, so round-trip through PG's
	// current_database(). Cheaper than threading state.
	var name string
	if err := d.QueryRowContext(context.Background(),
		`SELECT current_database()`).Scan(&name); err != nil {
		t.Fatalf("select current_database: %v", err)
	}
	return name
}

// provisionUserRoleSQL runs the three SQL statements the provisioner
// would emit, but with a caller-chosen password so tests can connect
// as the new role. Skips the vault call chain (entity lookup +
// static-role registration). Returns the role name.
//
// Production derives the role name from vault's entity ID
// (`user_<uuid>`) — tests don't have a vault talking to a real OIDC
// auth mount, so we synthesize a stable pseudo-entity-ID from the sub
// instead. The shape ("user_" + opaque id) is what matters downstream
// for RLS / grant-chain assertions.
func provisionUserRoleSQL(t *testing.T, d *db.DB, sub, password string) string {
	t.Helper()
	roleName := syntheticRoleName(sub)
	if err := ensureUserRole(context.Background(), d, roleName, password); err != nil {
		t.Fatalf("ensureUserRole: %v", err)
	}
	// Upsert a users row for the sub then persist pg_role, same as
	// the full provisioner would.
	_, err := d.ExecContext(context.Background(),
		`INSERT INTO blockyard.users (sub, email, name, last_login)
         VALUES ($1, $2, $3, now())
         ON CONFLICT (sub) DO UPDATE SET email = EXCLUDED.email`,
		sub, sub+"@example.com", sub)
	if err != nil {
		t.Fatalf("insert users row: %v", err)
	}
	if err := d.SetUserPgRole(context.Background(), sub, roleName); err != nil {
		t.Fatalf("SetUserPgRole: %v", err)
	}
	return roleName
}

// syntheticRoleName mimics the production `user_<entity-id>` shape
// using a sha256 slice of the sub instead of a real vault entity UUID.
// Only used by helpers that bypass the vault entity lookup.
func syntheticRoleName(sub string) string {
	h := sha256.Sum256([]byte(sub))
	return "user_" + hex.EncodeToString(h[:8])
}

// bootstrapAdmin ensures blockyard_admin exists. Separate helper so
// per-test assertions can verify idempotence.
func bootstrapAdmin(t *testing.T, d *db.DB) {
	t.Helper()
	if err := EnsureBlockyardAdmin(context.Background(), d); err != nil {
		t.Fatalf("EnsureBlockyardAdmin: %v", err)
	}
}

// ensureNoRows asserts q returns zero rows under the given
// connection. Used by RLS tests to assert filtering.
func ensureNoRows(t *testing.T, conn *sql.DB, q string, args ...any) {
	t.Helper()
	rows, err := conn.Query(q, args...)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatalf("expected 0 rows from %q; got at least one", q)
	}
}

// expectErrContains runs exec and asserts the error message contains
// substr (case-insensitive). Used to spot-check "permission denied"
// and "restrict_violation" error paths without binding on PG error
// codes that vary across versions.
func expectErrContains(t *testing.T, conn *sql.DB, stmt, substr string) {
	t.Helper()
	_, err := conn.Exec(stmt)
	if err == nil {
		t.Fatalf("expected error containing %q; stmt %q succeeded", substr, stmt)
	}
	if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(substr)) {
		t.Fatalf("error mismatch: stmt %q: %q does not contain %q",
			stmt, err.Error(), substr)
	}
}

// currentDB is a re-exported version to keep "db name from URL" off
// the hot path of each subtest.
func currentDB(t *testing.T, d *db.DB) string { return dbNameFromURL(t, d) }
