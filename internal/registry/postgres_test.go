package registry

import (
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

// pgTestBaseURL is the admin URL for the Postgres instance hosting the
// per-test databases. Empty when tests should skip.
var pgTestBaseURL string

// pgRegistryTemplate is the migrated template database. Per-test
// databases clone from it (CREATE DATABASE … TEMPLATE), which is
// orders of magnitude faster than re-running migrations. Own name
// (not reused from internal/db or internal/session) so the test
// binaries don't collide when CI runs them concurrently.
const pgRegistryTemplate = "blockyard_registry_test_template"

func TestMain(m *testing.M) {
	pgTestBaseURL = os.Getenv("BLOCKYARD_TEST_POSTGRES_URL")
	if pgTestBaseURL != "" {
		if err := setupRegistryTemplate(pgTestBaseURL); err != nil {
			fmt.Fprintf(os.Stderr, "registry: template bootstrap: %v\n", err)
			os.Exit(1)
		}
		defer teardownRegistryTemplate(pgTestBaseURL)
	}
	os.Exit(m.Run())
}

func setupRegistryTemplate(base string) error {
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return err
	}
	defer admin.Close()

	admin.Exec("DROP DATABASE IF EXISTS " + pgRegistryTemplate)
	if _, err := admin.Exec("CREATE DATABASE " + pgRegistryTemplate); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	// Serialize migration 001's CREATE ROLE against parallel test
	// packages — pg_authid is cluster-wide (#317).
	unlock := testutil.AcquirePGMigrationLockMain(base)
	tplURL := replacePGName(base, pgRegistryTemplate)
	tpl, err := db.Open(config.DatabaseConfig{Driver: "postgres", URL: tplURL})
	if err != nil {
		unlock()
		return fmt.Errorf("migrate template: %w", err)
	}
	tpl.Close()
	unlock()

	admin.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '" + pgRegistryTemplate + "'")
	admin.Exec("ALTER DATABASE " + pgRegistryTemplate + " WITH ALLOW_CONNECTIONS = false")
	return nil
}

func teardownRegistryTemplate(base string) {
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return
	}
	defer admin.Close()
	admin.Exec("ALTER DATABASE " + pgRegistryTemplate + " WITH ALLOW_CONNECTIONS = true")
	admin.Exec("DROP DATABASE IF EXISTS " + pgRegistryTemplate)
}

// testPGDB clones the pre-migrated template and returns a fresh
// *sqlx.DB pointing at it. Registers a t.Cleanup that drops the clone
// when the test exits.
func testPGDB(t *testing.T) *sqlx.DB {
	t.Helper()
	if pgTestBaseURL == "" {
		t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set; skipping Postgres registry tests")
	}

	dbName := "reg_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:20]

	admin, err := sql.Open("pgx", pgTestBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName + " TEMPLATE " + pgRegistryTemplate); err != nil {
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

func TestPostgresRegistryGetSetDelete(t *testing.T) {
	r := NewPostgresRegistry(testPGDB(t), time.Hour)

	r.Set("worker-1", "127.0.0.1:3838")

	addr, ok := r.Get("worker-1")
	if !ok {
		t.Fatal("expected worker to exist")
	}
	if addr != "127.0.0.1:3838" {
		t.Errorf("expected 127.0.0.1:3838, got %q", addr)
	}

	r.Delete("worker-1")
	if _, ok := r.Get("worker-1"); ok {
		t.Error("expected worker to be deleted")
	}
}

func TestPostgresRegistryGetMissing(t *testing.T) {
	r := NewPostgresRegistry(testPGDB(t), time.Hour)
	if _, ok := r.Get("nonexistent"); ok {
		t.Error("expected false for missing worker")
	}
}

// TestPostgresRegistrySetOverwrite confirms Set is idempotent — a
// second Set with a new address replaces the first. The Redis variant
// does the same via a straight SET; the Postgres path needs ON
// CONFLICT DO UPDATE, which this test pins.
func TestPostgresRegistrySetOverwrite(t *testing.T) {
	r := NewPostgresRegistry(testPGDB(t), time.Hour)

	r.Set("worker-1", "127.0.0.1:3838")
	r.Set("worker-1", "10.0.0.1:3838")

	addr, ok := r.Get("worker-1")
	if !ok {
		t.Fatal("expected worker to exist")
	}
	if addr != "10.0.0.1:3838" {
		t.Errorf("expected 10.0.0.1:3838 after overwrite, got %q", addr)
	}
}

// TestPostgresRegistryDeleteNonexistent confirms Delete is a no-op for
// a missing id (matches Redis DEL which returns 0 with no error).
func TestPostgresRegistryDeleteNonexistent(t *testing.T) {
	r := NewPostgresRegistry(testPGDB(t), time.Hour)
	// Should not panic or error.
	r.Delete("nonexistent")
}

// TestPostgresRegistryTTLExpiry confirms Get treats a stale heartbeat
// as "gone", matching the Redis TTL semantic. The health poller bumps
// last_heartbeat on every successful probe — here we starve that and
// push the clock forward via a direct UPDATE.
func TestPostgresRegistryTTLExpiry(t *testing.T) {
	db := testPGDB(t)
	r := NewPostgresRegistry(db, 10*time.Second)

	r.Set("worker-1", "127.0.0.1:3838")
	// Simulate 11 seconds without a heartbeat refresh.
	if _, err := db.Exec(
		`UPDATE blockyard_workers SET last_heartbeat = now() - interval '11 seconds' WHERE id = $1`,
		"worker-1",
	); err != nil {
		t.Fatal(err)
	}

	if _, ok := r.Get("worker-1"); ok {
		t.Error("expected registry entry to be stale and reported as gone")
	}
}

// TestPostgresRegistryTTLRefresh confirms Set bumps last_heartbeat —
// the mechanism the health poller uses to keep a worker alive in the
// registry.
func TestPostgresRegistryTTLRefresh(t *testing.T) {
	db := testPGDB(t)
	r := NewPostgresRegistry(db, 10*time.Second)

	r.Set("worker-1", "127.0.0.1:3838")
	if _, err := db.Exec(
		`UPDATE blockyard_workers SET last_heartbeat = now() - interval '6 seconds' WHERE id = $1`,
		"worker-1",
	); err != nil {
		t.Fatal(err)
	}

	// Re-Set refreshes the heartbeat (simulates health poller behaviour).
	r.Set("worker-1", "127.0.0.1:3838")

	addr, ok := r.Get("worker-1")
	if !ok {
		t.Error("expected worker to still exist after heartbeat refresh")
	}
	if addr != "127.0.0.1:3838" {
		t.Errorf("expected 127.0.0.1:3838, got %q", addr)
	}
}

// TestPostgresRegistryZeroTTLNeverExpires pins the "registryTTL == 0"
// contract: Get does not filter by heartbeat age. Matches the
// registry.WorkerRegistry callers in tests that don't care about TTL.
func TestPostgresRegistryZeroTTLNeverExpires(t *testing.T) {
	db := testPGDB(t)
	r := NewPostgresRegistry(db, 0)

	r.Set("worker-1", "127.0.0.1:3838")
	if _, err := db.Exec(
		`UPDATE blockyard_workers SET last_heartbeat = now() - interval '1 hour' WHERE id = $1`,
		"worker-1",
	); err != nil {
		t.Fatal(err)
	}

	if _, ok := r.Get("worker-1"); !ok {
		t.Error("expected worker to still exist with TTL=0")
	}
}
