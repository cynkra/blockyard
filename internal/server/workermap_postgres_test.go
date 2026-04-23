package server

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

	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgTestBaseURL is the admin URL for the Postgres instance hosting
// the per-test databases. Empty when tests should skip.
var pgTestBaseURL string

// pgWorkersTemplate is the migrated template database. Per-test
// databases clone from it (CREATE DATABASE … TEMPLATE) — orders of
// magnitude faster than re-running migrations. Own name so the test
// binaries don't collide when CI runs them concurrently.
const pgWorkersTemplate = "blockyard_workers_test_template"

func TestMain(m *testing.M) {
	pgTestBaseURL = os.Getenv("BLOCKYARD_TEST_POSTGRES_URL")
	if pgTestBaseURL != "" {
		if err := setupWorkersTemplate(pgTestBaseURL); err != nil {
			fmt.Fprintf(os.Stderr, "server: workers template bootstrap: %v\n", err)
			os.Exit(1)
		}
		defer teardownWorkersTemplate(pgTestBaseURL)
	}
	os.Exit(m.Run())
}

func setupWorkersTemplate(base string) error {
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return err
	}
	defer admin.Close()

	admin.Exec("DROP DATABASE IF EXISTS " + pgWorkersTemplate)
	if _, err := admin.Exec("CREATE DATABASE " + pgWorkersTemplate); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	tplURL := replacePGName(base, pgWorkersTemplate)
	tpl, err := db.Open(config.DatabaseConfig{Driver: "postgres", URL: tplURL})
	if err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}
	tpl.Close()

	admin.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '" + pgWorkersTemplate + "'")
	admin.Exec("ALTER DATABASE " + pgWorkersTemplate + " WITH ALLOW_CONNECTIONS = false")
	return nil
}

func teardownWorkersTemplate(base string) {
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return
	}
	defer admin.Close()
	admin.Exec("ALTER DATABASE " + pgWorkersTemplate + " WITH ALLOW_CONNECTIONS = true")
	admin.Exec("DROP DATABASE IF EXISTS " + pgWorkersTemplate)
}

func testPGDB(t *testing.T) *sqlx.DB {
	t.Helper()
	if pgTestBaseURL == "" {
		t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set; skipping Postgres worker map tests")
	}

	dbName := "wm_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:20]

	admin, err := sql.Open("pgx", pgTestBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName + " TEMPLATE " + pgWorkersTemplate); err != nil {
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

// TestPostgresWorkerMapServerIDHardcoded pins that Set always stamps
// the serverID given at construction time — not whatever ActiveWorker
// field might carry. This matches RedisWorkerMap behavior: WorkersForServer
// filters by the construction-time serverID.
func TestPostgresWorkerMapServerIDHardcoded(t *testing.T) {
	m := NewPostgresWorkerMap(testPGDB(t), "host-A")
	m.Set("w1", ActiveWorker{AppID: "app1"})

	if ids := m.WorkersForServer("host-A"); len(ids) != 1 {
		t.Errorf("WorkersForServer(host-A) = %v, want [w1]", ids)
	}
	if ids := m.WorkersForServer("host-B"); len(ids) != 0 {
		t.Errorf("WorkersForServer(host-B) = %v, want empty", ids)
	}
}

// TestPostgresWorkerMapIdleSinceNullRoundTrip pins the Go-to-SQL
// mapping for idle_since: Go time.Time{} (zero) ↔ SQL NULL. The Get
// path must hand back a zero-valued IdleSince (not sql.NullTime with
// Valid=false) so callers that check w.IdleSince.IsZero() keep working.
func TestPostgresWorkerMapIdleSinceNullRoundTrip(t *testing.T) {
	m := NewPostgresWorkerMap(testPGDB(t), "test-host")
	m.Set("w1", ActiveWorker{AppID: "app1"})

	w, ok := m.Get("w1")
	if !ok {
		t.Fatal("expected worker to exist")
	}
	if !w.IdleSince.IsZero() {
		t.Errorf("IdleSince = %v, want zero value", w.IdleSince)
	}
}

// TestPostgresWorkerMapReaper covers the crashed-pod cleanup path: a
// worker whose last_heartbeat falls past the reaper threshold gets its
// row deleted so WorkerMap aggregate queries don't report a ghost.
// Mirrors the session RunExpiry contract but operates on the shared
// blockyard_workers table instead of a dedicated sessions table.
func TestPostgresWorkerMapReaper(t *testing.T) {
	db := testPGDB(t)
	m := NewPostgresWorkerMap(db, "test-host")

	m.Set("w-alive", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})
	m.Set("w-dead", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})

	// Push w-dead's heartbeat past the reaper threshold. w-alive's stays
	// fresh (now()).
	if _, err := db.Exec(
		`UPDATE blockyard_workers SET last_heartbeat = now() - interval '10 minutes' WHERE id = $1`,
		"w-dead",
	); err != nil {
		t.Fatal(err)
	}

	m.reapStale(context.Background(), 5*time.Minute)

	if _, ok := m.Get("w-dead"); ok {
		t.Error("stale worker should be reaped")
	}
	if _, ok := m.Get("w-alive"); !ok {
		t.Error("fresh worker must survive the reaper")
	}
}

// TestPostgresWorkerMapReaperZeroThresholdIsNoOp pins the
// "disable reaper via threshold=0" contract — callers that skip the
// reaper (e.g. single-pod debug runs) shouldn't have to not-schedule
// it, they can schedule it with 0 and get a cheap early-return.
func TestPostgresWorkerMapReaperZeroThresholdIsNoOp(t *testing.T) {
	db := testPGDB(t)
	m := NewPostgresWorkerMap(db, "test-host")

	m.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})
	if _, err := db.Exec(
		`UPDATE blockyard_workers SET last_heartbeat = now() - interval '1 hour' WHERE id = $1`,
		"w1",
	); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // RunReaper returns immediately on threshold <= 0
	m.RunReaper(ctx, 0, time.Millisecond)

	if _, ok := m.Get("w1"); !ok {
		t.Error("threshold=0 should leave stale rows alone")
	}
}

// TestPostgresWorkerMapSetUpsertPreservesAddress verifies that
// PostgresWorkerMap.Set does not clobber the address column that
// registry.PostgresRegistry.Set writes to the same row. The two stores
// share the table and each must update only its own columns.
func TestPostgresWorkerMapSetUpsertPreservesAddress(t *testing.T) {
	db := testPGDB(t)
	m := NewPostgresWorkerMap(db, "test-host")

	// Simulate a prior Registry.Set that populated the address column.
	if _, err := db.Exec(
		`INSERT INTO blockyard_workers (id, address, last_heartbeat)
		 VALUES ($1, $2, now())`,
		"w1", "127.0.0.1:3838",
	); err != nil {
		t.Fatal(err)
	}

	m.Set("w1", ActiveWorker{AppID: "app1", BundleID: "b1", StartedAt: time.Now()})

	// Address must still be there.
	var addr string
	if err := db.QueryRow(
		`SELECT address FROM blockyard_workers WHERE id = $1`, "w1",
	).Scan(&addr); err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:3838" {
		t.Errorf("address = %q, want %q (WorkerMap.Set must not touch address)", addr, "127.0.0.1:3838")
	}
}
