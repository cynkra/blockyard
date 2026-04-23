package process

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgTestBaseURL is the admin URL for the Postgres instance hosting the
// per-test databases. Empty when tests should skip.
var pgTestBaseURL string

// pgAllocatorsTemplate is the migrated template database. Per-test
// databases clone from it (CREATE DATABASE … TEMPLATE) — orders of
// magnitude faster than re-running migrations. Own name (not reused
// from internal/db, internal/session, internal/registry, internal/server)
// so the test binaries don't collide when CI runs them concurrently.
const pgAllocatorsTemplate = "blockyard_allocators_test_template"

func TestMain(m *testing.M) {
	pgTestBaseURL = os.Getenv("BLOCKYARD_TEST_POSTGRES_URL")
	if pgTestBaseURL != "" {
		if err := setupAllocatorsTemplate(pgTestBaseURL); err != nil {
			fmt.Fprintf(os.Stderr, "process: allocators template bootstrap: %v\n", err)
			os.Exit(1)
		}
		defer teardownAllocatorsTemplate(pgTestBaseURL)
	}
	os.Exit(m.Run())
}

func setupAllocatorsTemplate(base string) error {
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return err
	}
	defer admin.Close()

	admin.Exec("DROP DATABASE IF EXISTS " + pgAllocatorsTemplate)
	if _, err := admin.Exec("CREATE DATABASE " + pgAllocatorsTemplate); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	tplURL := replacePGName(base, pgAllocatorsTemplate)
	tpl, err := db.Open(config.DatabaseConfig{Driver: "postgres", URL: tplURL})
	if err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}
	tpl.Close()

	admin.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '" + pgAllocatorsTemplate + "'")
	admin.Exec("ALTER DATABASE " + pgAllocatorsTemplate + " WITH ALLOW_CONNECTIONS = false")
	return nil
}

func teardownAllocatorsTemplate(base string) {
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return
	}
	defer admin.Close()
	admin.Exec("ALTER DATABASE " + pgAllocatorsTemplate + " WITH ALLOW_CONNECTIONS = true")
	admin.Exec("DROP DATABASE IF EXISTS " + pgAllocatorsTemplate)
}

// testPGDB clones the pre-migrated template and returns a fresh
// *sqlx.DB pointing at it. Registers a t.Cleanup that drops the clone
// when the test exits.
func testPGDB(t *testing.T) *sqlx.DB {
	t.Helper()
	if pgTestBaseURL == "" {
		t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set; skipping Postgres allocator tests")
	}

	dbName := "alloc_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:20]

	admin, err := sql.Open("pgx", pgTestBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName + " TEMPLATE " + pgAllocatorsTemplate); err != nil {
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
