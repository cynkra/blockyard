package session

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/testutil"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgTestBaseURL is the admin URL for the Postgres instance hosting
// the per-test databases. Empty when tests should skip.
var pgTestBaseURL string

// pgSessionsTemplate is the name of the migrated template database
// created in TestMain. Per-test databases clone from it (CREATE
// DATABASE … TEMPLATE) which is orders of magnitude faster than
// re-running migrations. Own name (not reused from internal/db) so
// the two test binaries don't collide when CI runs them concurrently.
const pgSessionsTemplate = "blockyard_sessions_test_template"

func TestMain(m *testing.M) {
	pgTestBaseURL = os.Getenv("BLOCKYARD_TEST_POSTGRES_URL")
	if pgTestBaseURL != "" {
		if err := setupSessionsTemplate(pgTestBaseURL); err != nil {
			fmt.Fprintf(os.Stderr, "session: template bootstrap: %v\n", err)
			os.Exit(1)
		}
		defer teardownSessionsTemplate(pgTestBaseURL)
	}
	os.Exit(m.Run())
}

func setupSessionsTemplate(base string) error {
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return err
	}
	defer admin.Close()

	admin.Exec("DROP DATABASE IF EXISTS " + pgSessionsTemplate)
	if _, err := admin.Exec("CREATE DATABASE " + pgSessionsTemplate); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	// Serialize migration 001's CREATE ROLE against parallel test
	// packages — pg_authid is cluster-wide (#317).
	unlock := testutil.AcquirePGMigrationLockMain(base)
	tplURL := replacePGName(base, pgSessionsTemplate)
	tpl, err := db.Open(config.DatabaseConfig{Driver: "postgres", URL: tplURL})
	if err != nil {
		unlock()
		return fmt.Errorf("migrate template: %w", err)
	}
	tpl.Close()
	unlock()

	// Kick idle pool connections and mark template non-connectable so
	// CREATE DATABASE … TEMPLATE succeeds (filesystem copy; does not
	// need a live connection to the template).
	admin.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '" + pgSessionsTemplate + "'")
	admin.Exec("ALTER DATABASE " + pgSessionsTemplate + " WITH ALLOW_CONNECTIONS = false")
	return nil
}

func teardownSessionsTemplate(base string) {
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return
	}
	defer admin.Close()
	admin.Exec("ALTER DATABASE " + pgSessionsTemplate + " WITH ALLOW_CONNECTIONS = true")
	admin.Exec("DROP DATABASE IF EXISTS " + pgSessionsTemplate)
}

// testPGDB clones the pre-migrated template and returns a fresh
// *sqlx.DB pointing at it. Registers a t.Cleanup that drops the
// clone when the test exits.
func testPGDB(t *testing.T) *sqlx.DB {
	t.Helper()
	if pgTestBaseURL == "" {
		t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set; skipping Postgres session tests")
	}

	dbName := "sess_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:20]

	admin, err := sql.Open("pgx", pgTestBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName + " TEMPLATE " + pgSessionsTemplate); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Close()

	testURL := replacePGName(pgTestBaseURL, dbName)
	rawDB, err := sqlx.Open("pgx", testURL)
	if err != nil {
		t.Fatal(err)
	}
	rawDB.SetMaxOpenConns(5)

	t.Cleanup(func() {
		rawDB.Close()
		cleanup, cErr := sql.Open("pgx", pgTestBaseURL)
		if cErr == nil {
			cleanup.Exec("DROP DATABASE IF EXISTS " + dbName)
			cleanup.Close()
		}
	})
	return rawDB
}

func replacePGName(raw, name string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Path = "/" + name
	return u.String()
}

func TestPostgresStoreGetSetRoundTrip(t *testing.T) {
	s := NewPostgresStore(testPGDB(t), time.Hour)

	now := time.Now().UTC().Truncate(time.Microsecond)
	s.Set("sess-1", Entry{WorkerID: "w1", UserSub: "user-a", LastAccess: now})

	e, ok := s.Get("sess-1")
	if !ok {
		t.Fatal("expected session to exist")
	}
	if e.WorkerID != "w1" || e.UserSub != "user-a" {
		t.Errorf("unexpected entry: %+v", e)
	}
	if !e.LastAccess.Equal(now) {
		t.Errorf("LastAccess = %v, want %v", e.LastAccess, now)
	}
}

func TestPostgresStoreSetEmptyUserSub(t *testing.T) {
	// UserSub defaults to '' in the schema; the proxy writes empty when
	// OIDC is not configured. Round-trip must preserve that.
	s := NewPostgresStore(testPGDB(t), time.Hour)
	s.Set("sess-1", Entry{WorkerID: "w1", LastAccess: time.Now()})

	e, ok := s.Get("sess-1")
	if !ok {
		t.Fatal("expected session to exist")
	}
	if e.UserSub != "" {
		t.Errorf("UserSub = %q, want empty", e.UserSub)
	}
}

func TestPostgresStoreRunExpiryDeletesPastRows(t *testing.T) {
	// Set idleTTL very small so expires_at lands in the past quickly.
	s := NewPostgresStore(testPGDB(t), 10*time.Millisecond)

	s.Set("sess-old", Entry{WorkerID: "w1", LastAccess: time.Now()})
	time.Sleep(50 * time.Millisecond)

	// Manually call the internal sweep rather than running the goroutine
	// — avoids racing with ticker cadence.
	s.sweepExpired(context.Background())

	if _, ok := s.Get("sess-old"); ok {
		t.Error("expected expired session to be gone")
	}
}

func TestPostgresStoreRunExpiryKeepsFreshRows(t *testing.T) {
	s := NewPostgresStore(testPGDB(t), time.Hour)

	s.Set("sess-fresh", Entry{WorkerID: "w1", LastAccess: time.Now()})
	s.sweepExpired(context.Background())

	if _, ok := s.Get("sess-fresh"); !ok {
		t.Error("fresh session should survive sweep")
	}
}

func TestPostgresStoreSetOverwriteUpdatesExpiry(t *testing.T) {
	// Second Set with a later LastAccess must push expires_at forward.
	// Otherwise reroute/re-set on a long-lived session would re-use the
	// original (near-past) expiry.
	s := NewPostgresStore(testPGDB(t), time.Hour)

	old := time.Now().Add(-30 * time.Minute)
	s.Set("sess-1", Entry{WorkerID: "w1", LastAccess: old})

	now := time.Now()
	s.Set("sess-1", Entry{WorkerID: "w2", LastAccess: now})

	// Read expires_at directly — the Store interface doesn't expose it.
	var expiresAt time.Time
	err := s.db.QueryRowx(
		`SELECT expires_at FROM blockyard_sessions WHERE id = $1`, "sess-1",
	).Scan(&expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	// expiresAt should be ~ now + 1h, not old + 1h.
	minExpected := now.Add(59 * time.Minute)
	if expiresAt.Before(minExpected) {
		t.Errorf("expires_at = %v, want >= %v", expiresAt, minExpected)
	}
}

func TestPostgresStoreZeroTTLDoesNotExpire(t *testing.T) {
	// idleTTL == 0 means "no automatic expiry"; sweepExpired must not
	// delete the row.
	s := NewPostgresStore(testPGDB(t), 0)
	s.Set("sess-immortal", Entry{WorkerID: "w1", LastAccess: time.Now()})
	s.sweepExpired(context.Background())

	if _, ok := s.Get("sess-immortal"); !ok {
		t.Error("zero-TTL session should never be swept")
	}
}
