package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// pgTemplateDB is the name of the pre-migrated template database, created
// once in TestMain.  Each postgres subtest clones it via CREATE DATABASE …
// TEMPLATE, which is near-instant compared to running migrations every time.
const pgTemplateDB = "blockyard_test_template"

// pgBaseURL is the postgres connection URL, set once in TestMain.
var pgBaseURL string

func TestMain(m *testing.M) {
	pgBaseURL = os.Getenv("BLOCKYARD_TEST_POSTGRES_URL")
	if pgBaseURL != "" {
		// Create (or replace) the template database once for the whole
		// test binary.  This runs migrations a single time.
		admin, err := sql.Open("pgx", pgBaseURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "postgres admin connect: %v\n", err)
			os.Exit(1)
		}
		// Drop any stale template from a previous interrupted run.
		admin.Exec("DROP DATABASE IF EXISTS " + pgTemplateDB)
		if _, err := admin.Exec("CREATE DATABASE " + pgTemplateDB); err != nil {
			fmt.Fprintf(os.Stderr, "create template db: %v\n", err)
			os.Exit(1)
		}
		admin.Close()

		// Open via our normal path so migrations run against the template.
		tplURL := replaceDBName(pgBaseURL, pgTemplateDB)
		tpl, err := Open(config.DatabaseConfig{Driver: "postgres", URL: tplURL})
		if err != nil {
			fmt.Fprintf(os.Stderr, "migrate template db: %v\n", err)
			os.Exit(1)
		}
		tpl.Close()

		// Prevent the "source database is being accessed by other users"
		// error: kill any lingering pool connections and disallow future
		// connections.  CREATE DATABASE … TEMPLATE still works because it
		// copies at the filesystem level without connecting.
		admin2, err := sql.Open("pgx", pgBaseURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "postgres admin reconnect: %v\n", err)
			os.Exit(1)
		}
		admin2.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '" + pgTemplateDB + "'")
		admin2.Exec("ALTER DATABASE " + pgTemplateDB + " WITH ALLOW_CONNECTIONS = false")
		admin2.Close()
	}

	code := m.Run()

	// Tear down the template database.
	if pgBaseURL != "" {
		admin, err := sql.Open("pgx", pgBaseURL)
		if err == nil {
			admin.Exec("ALTER DATABASE " + pgTemplateDB + " WITH ALLOW_CONNECTIONS = true")
			admin.Exec("DROP DATABASE IF EXISTS " + pgTemplateDB)
			admin.Close()
		}
	}
	os.Exit(code)
}

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testPostgresDB(t *testing.T) *DB {
	t.Helper()

	if pgBaseURL == "" {
		t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set; skipping PostgreSQL tests")
	}

	// Clone the pre-migrated template — much faster than running migrations.
	dbName := "test_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]

	admin, err := sql.Open("pgx", pgBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName + " TEMPLATE " + pgTemplateDB); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Close()

	// Open without running migrations — the schema is already in place.
	testURL := replaceDBName(pgBaseURL, dbName)
	rawDB, err := sqlx.Open("pgx", testURL)
	if err != nil {
		t.Fatal(err)
	}
	rawDB.SetMaxOpenConns(5)
	db := &DB{DB: rawDB, dialect: DialectPostgres, connURL: testURL}

	t.Cleanup(func() {
		db.Close()
		cleanup, _ := sql.Open("pgx", pgBaseURL)
		cleanup.Exec("DROP DATABASE IF EXISTS " + dbName)
		cleanup.Close()
	})
	return db
}

// replaceDBName replaces the database name in a PostgreSQL URL.
func replaceDBName(rawURL, newDB string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.Path = "/" + newDB
	return u.String()
}

// eachDB runs a test function against both SQLite and PostgreSQL.
// Subtests run in parallel — each uses an isolated database.
func eachDB(t *testing.T, fn func(t *testing.T, db *DB)) {
	t.Run("sqlite", func(t *testing.T) {
		t.Parallel()
		fn(t, testDB(t))
	})
	t.Run("postgres", func(t *testing.T) {
		t.Parallel()
		fn(t, testPostgresDB(t))
	})
}

func TestCreateAndGetApp(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, err := db.CreateApp("my-app", "admin")
		if err != nil {
			t.Fatal(err)
		}
		if app.Name != "my-app" {
			t.Errorf("expected my-app, got %q", app.Name)
		}
		if app.ID == "" {
			t.Error("expected non-empty ID")
		}

		fetched, err := db.GetApp(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fetched.ID != app.ID {
			t.Errorf("expected %q, got %q", app.ID, fetched.ID)
		}
	})
}

func TestGetAppByName(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")

		fetched, err := db.GetAppByName("my-app")
		if err != nil {
			t.Fatal(err)
		}
		if fetched.ID != app.ID {
			t.Errorf("expected %q, got %q", app.ID, fetched.ID)
		}

		missing, err := db.GetAppByName("nonexistent")
		if err != nil {
			t.Fatal(err)
		}
		if missing != nil {
			t.Error("expected nil for nonexistent app")
		}
	})
}

func TestDuplicateNameFails(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		_, err := db.CreateApp("my-app", "admin")
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.CreateApp("my-app", "admin")
		if err == nil {
			t.Error("expected error on duplicate name")
		}
	})
}

func TestDeleteApp(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")
		err := db.HardDeleteApp(app.ID)
		if err != nil {
			t.Fatal(err)
		}

		fetched, _ := db.GetApp(app.ID)
		if fetched != nil {
			t.Error("expected nil after deletion")
		}
	})
}

func TestListApps(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		db.CreateApp("app-a", "admin")
		db.CreateApp("app-b", "admin")

		apps, err := db.ListApps()
		if err != nil {
			t.Fatal(err)
		}
		if len(apps) != 2 {
			t.Errorf("expected 2 apps, got %d", len(apps))
		}
	})
}

func TestCreateAndGetBundle(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")

		b, err := db.CreateBundle("b-1", app.ID, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != "pending" {
			t.Errorf("expected pending, got %q", b.Status)
		}
		if b.AppID != app.ID {
			t.Errorf("expected app ID %q, got %q", app.ID, b.AppID)
		}

		fetched, err := db.GetBundle("b-1")
		if err != nil {
			t.Fatal(err)
		}
		if fetched.ID != "b-1" {
			t.Errorf("expected b-1, got %q", fetched.ID)
		}
	})
}

func TestListBundlesByApp(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")

		db.CreateBundle("b-1", app.ID, "", false)
		db.CreateBundle("b-2", app.ID, "", false)

		bundles, err := db.ListBundlesByApp(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(bundles) != 2 {
			t.Errorf("expected 2 bundles, got %d", len(bundles))
		}
	})
}

func TestUpdateBundleStatus(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")
		db.CreateBundle("b-1", app.ID, "", false)

		if err := db.UpdateBundleStatus("b-1", "building"); err != nil {
			t.Fatal(err)
		}

		b, _ := db.GetBundle("b-1")
		if b.Status != "building" {
			t.Errorf("expected building, got %q", b.Status)
		}
	})
}

func TestSetActiveBundle(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")
		db.CreateBundle("b-1", app.ID, "", false)

		if err := db.SetActiveBundle(app.ID, "b-1"); err != nil {
			t.Fatal(err)
		}

		fetched, _ := db.GetApp(app.ID)
		if fetched.ActiveBundle == nil || *fetched.ActiveBundle != "b-1" {
			t.Errorf("expected active bundle b-1, got %v", fetched.ActiveBundle)
		}
	})
}

func TestDeleteBundle(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")
		db.CreateBundle("b-1", app.ID, "", false)

		deleted, err := db.DeleteBundle("b-1")
		if err != nil {
			t.Fatal(err)
		}
		if !deleted {
			t.Error("expected deletion")
		}

		fetched, _ := db.GetBundle("b-1")
		if fetched != nil {
			t.Error("expected nil after deletion")
		}
	})
}

func TestUpdateApp(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")

		mem := "512m"
		cpu := 2.0
		workers := 3
		updated, err := db.UpdateApp(app.ID, AppUpdate{
			MemoryLimit:      &mem,
			CPULimit:         &cpu,
			MaxWorkersPerApp: &workers,
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.MemoryLimit == nil || *updated.MemoryLimit != "512m" {
			t.Errorf("expected memory_limit=512m, got %v", updated.MemoryLimit)
		}
		if updated.CPULimit == nil || *updated.CPULimit != 2.0 {
			t.Errorf("expected cpu_limit=2.0, got %v", updated.CPULimit)
		}
		if updated.MaxWorkersPerApp == nil || *updated.MaxWorkersPerApp != 3 {
			t.Errorf("expected max_workers_per_app=3, got %v", updated.MaxWorkersPerApp)
		}
		// Unchanged field should keep default
		if updated.MaxSessionsPerWorker != 1 {
			t.Errorf("expected max_sessions_per_worker=1, got %d", updated.MaxSessionsPerWorker)
		}
	})
}

func TestUpdateAppNotFound(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		_, err := db.UpdateApp("nonexistent", AppUpdate{})
		if err == nil {
			t.Error("expected error for nonexistent app")
		}
	})
}

func TestClearActiveBundle(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")
		db.CreateBundle("b-1", app.ID, "", false)
		db.SetActiveBundle(app.ID, "b-1")

		// Verify it's set
		fetched, _ := db.GetApp(app.ID)
		if fetched.ActiveBundle == nil {
			t.Fatal("expected active bundle to be set")
		}

		// Clear it
		if err := db.ClearActiveBundle(app.ID); err != nil {
			t.Fatal(err)
		}

		fetched, _ = db.GetApp(app.ID)
		if fetched.ActiveBundle != nil {
			t.Errorf("expected nil active bundle, got %v", fetched.ActiveBundle)
		}
	})
}

func TestFailStaleBuilds(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")

		// Insert a bundle in "building" state
		_, err := db.Exec(db.rebind(
			`INSERT INTO bundles (id, app_id, status, uploaded_at)
			 VALUES (?, ?, 'building', '2024-01-01T00:00:00Z')`),
			"b1", app.ID,
		)
		if err != nil {
			t.Fatal(err)
		}

		n, err := db.FailStaleBuilds()
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("expected 1 stale build marked failed, got %d", n)
		}

		// Verify status changed
		b, _ := db.GetBundle("b1")
		if b.Status != "failed" {
			t.Errorf("expected 'failed', got %q", b.Status)
		}
	})
}

func TestOpenCreatesDirectory(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "subdir", "nested", "test.db")

	database, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	// Directory should have been created
	dir := filepath.Dir(dbPath)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("expected directory to exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected a directory")
	}

	// DB should be functional
	app, err := database.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != "test-app" {
		t.Errorf("expected test-app, got %q", app.Name)
	}
}

func TestOpenInvalidPath(t *testing.T) {
	// Try to open a DB inside a file (not a directory) — should fail
	tmp := t.TempDir()
	filePath := filepath.Join(tmp, "afile")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Attempt to create a DB at afile/sub/test.db — MkdirAll should fail
	// because "afile" is a regular file, not a directory.
	dbPath := filepath.Join(filePath, "sub", "test.db")
	_, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err == nil {
		t.Fatal("expected error opening DB under a file path")
	}
}

func TestListCatalogAdmin(t *testing.T) {
	eachDB(t, func(t *testing.T, database *DB) {
		database.CreateApp("app-a", "user-1")
		database.CreateApp("app-b", "user-2")
		database.CreateApp("app-c", "user-3")

		// Admin sees all apps
		apps, total, err := database.ListCatalog(CatalogParams{
			CallerRole: "admin",
			Page:       1,
			PerPage:    10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 3 {
			t.Errorf("expected total=3, got %d", total)
		}
		if len(apps) != 3 {
			t.Errorf("expected 3 apps, got %d", len(apps))
		}
	})
}

func TestListCatalogUnauthenticated(t *testing.T) {
	eachDB(t, func(t *testing.T, database *DB) {
		app1, _ := database.CreateApp("public-app", "user-1")
		database.UpdateApp(app1.ID, AppUpdate{AccessType: strPtr("public")})
		database.CreateApp("private-app", "user-2")

		// Unauthenticated caller sees only public apps
		apps, total, err := database.ListCatalog(CatalogParams{
			Page:    1,
			PerPage: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			t.Errorf("expected total=1, got %d", total)
		}
		if len(apps) != 1 {
			t.Errorf("expected 1 app, got %d", len(apps))
		}
	})
}

func TestListCatalogOwnerFilter(t *testing.T) {
	eachDB(t, func(t *testing.T, database *DB) {
		database.CreateApp("my-app", "user-1")
		database.CreateApp("other-app", "user-2")

		// Authenticated non-admin sees own apps
		apps, total, err := database.ListCatalog(CatalogParams{
			CallerSub: "user-1",
			Page:      1,
			PerPage:   10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			t.Errorf("expected total=1, got %d", total)
		}
		if len(apps) != 1 {
			t.Errorf("expected 1 app, got %d", len(apps))
		}
	})
}

func TestListCatalogSearch(t *testing.T) {
	eachDB(t, func(t *testing.T, database *DB) {
		database.CreateApp("shiny-dashboard", "admin")
		database.CreateApp("plumber-api", "admin")

		apps, total, err := database.ListCatalog(CatalogParams{
			CallerRole: "admin",
			Search:     "shiny",
			Page:       1,
			PerPage:    10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			t.Errorf("expected total=1, got %d", total)
		}
		if len(apps) != 1 {
			t.Errorf("expected 1 app, got %d", len(apps))
		}
	})
}

func TestListCatalogPagination(t *testing.T) {
	eachDB(t, func(t *testing.T, database *DB) {
		for i := 0; i < 5; i++ {
			database.CreateApp(fmt.Sprintf("app-%d", i), "admin")
		}

		// Page 1 with 2 per page
		apps, total, err := database.ListCatalog(CatalogParams{
			CallerRole: "admin",
			Page:       1,
			PerPage:    2,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 5 {
			t.Errorf("expected total=5, got %d", total)
		}
		if len(apps) != 2 {
			t.Errorf("expected 2 apps on page 1, got %d", len(apps))
		}

		// Page 3 with 2 per page — should get 1 app
		apps, _, err = database.ListCatalog(CatalogParams{
			CallerRole: "admin",
			Page:       3,
			PerPage:    2,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(apps) != 1 {
			t.Errorf("expected 1 app on page 3, got %d", len(apps))
		}
	})
}

func TestListCatalogTagFilter(t *testing.T) {
	eachDB(t, func(t *testing.T, database *DB) {
		app1, _ := database.CreateApp("tagged-app", "admin")
		database.CreateApp("untagged-app", "admin")

		tag, err := database.CreateTag("production")
		if err != nil {
			t.Fatal(err)
		}
		database.AddAppTag(app1.ID, tag.ID)

		apps, total, err := database.ListCatalog(CatalogParams{
			CallerRole: "admin",
			Tag:        "production",
			Page:       1,
			PerPage:    10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			t.Errorf("expected total=1, got %d", total)
		}
		if len(apps) != 1 {
			t.Errorf("expected 1 app, got %d", len(apps))
		}
	})
}

func TestListCatalogLoggedIn(t *testing.T) {
	eachDB(t, func(t *testing.T, database *DB) {
		app1, _ := database.CreateApp("logged-in-app", "owner-1")
		database.UpdateApp(app1.ID, AppUpdate{AccessType: strPtr("logged_in")})
		database.CreateApp("private-app", "owner-2")

		// Authenticated user sees logged_in apps even without explicit grant.
		apps, total, err := database.ListCatalog(CatalogParams{
			CallerSub: "user-x",
			Page:      1,
			PerPage:   10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			t.Errorf("expected total=1, got %d", total)
		}
		if len(apps) != 1 {
			t.Errorf("expected 1 app, got %d", len(apps))
		}
	})
}

func strPtr(s string) *string { return &s }

func hashPAT(plaintext string) []byte {
	h := sha256.Sum256([]byte(plaintext))
	return h[:]
}

func TestOpenMemory(t *testing.T) {
	db, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open(:memory:) failed: %v", err)
	}
	defer db.Close()

	// Verify functional by creating an app.
	app, err := db.CreateApp("mem-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != "mem-app" {
		t.Errorf("expected mem-app, got %q", app.Name)
	}
}

func TestPing(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		if err := db.Ping(ctx); err != nil {
			t.Fatalf("Ping failed: %v", err)
		}
	})
}

func TestIsUniqueConstraintError(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		_, err := db.CreateApp("dup-app", "admin")
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.CreateApp("dup-app", "admin")
		if !IsUniqueConstraintError(err) {
			t.Errorf("expected unique constraint error, got %v", err)
		}

		// nil error should return false.
		if IsUniqueConstraintError(nil) {
			t.Error("expected false for nil error")
		}

		// Unrelated error should return false.
		if IsUniqueConstraintError(fmt.Errorf("some other error")) {
			t.Error("expected false for unrelated error")
		}
	})
}

func TestGetAppNonexistent(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, err := db.GetApp("00000000-0000-0000-0000-000000000000")
		if err != nil {
			t.Fatal(err)
		}
		if app != nil {
			t.Error("expected nil for nonexistent app")
		}
	})
}

func TestGetBundleNonexistent(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		bundle, err := db.GetBundle("nonexistent-bundle-id")
		if err != nil {
			t.Fatal(err)
		}
		if bundle != nil {
			t.Error("expected nil for nonexistent bundle")
		}
	})
}

func TestDeleteAppNonexistent(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		err := db.HardDeleteApp("00000000-0000-0000-0000-000000000000")
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestDeleteBundleNonexistent(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		deleted, err := db.DeleteBundle("nonexistent-bundle-id")
		if err != nil {
			t.Fatal(err)
		}
		if deleted {
			t.Error("expected false for nonexistent bundle")
		}
	})
}

func TestDeleteTagNonexistent(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		deleted, err := db.DeleteTag("nonexistent-tag-id")
		if err != nil {
			t.Fatal(err)
		}
		if deleted {
			t.Error("expected false for nonexistent tag")
		}
	})
}

func TestUserCRUD(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		// UpsertUser creates a new user with default role.
		user, err := db.UpsertUser("sub-1", "alice@example.com", "Alice")
		if err != nil {
			t.Fatal(err)
		}
		if user.Sub != "sub-1" {
			t.Errorf("expected sub-1, got %q", user.Sub)
		}
		if user.Email != "alice@example.com" {
			t.Errorf("expected alice@example.com, got %q", user.Email)
		}
		if user.Role != "viewer" {
			t.Errorf("expected default role viewer, got %q", user.Role)
		}
		if !user.Active {
			t.Error("expected new user to be active")
		}

		// GetUser retrieves the user.
		fetched, err := db.GetUser("sub-1")
		if err != nil {
			t.Fatal(err)
		}
		if fetched == nil || fetched.Sub != "sub-1" {
			t.Errorf("expected user sub-1, got %v", fetched)
		}

		// GetUser returns nil for nonexistent user.
		missing, err := db.GetUser("nonexistent")
		if err != nil {
			t.Fatal(err)
		}
		if missing != nil {
			t.Error("expected nil for nonexistent user")
		}

		// UpsertUser on existing user preserves role and active.
		db.UpdateUser("sub-1", UserUpdate{Role: strPtr("admin")})
		user, err = db.UpsertUser("sub-1", "alice-new@example.com", "Alice Updated")
		if err != nil {
			t.Fatal(err)
		}
		if user.Email != "alice-new@example.com" {
			t.Errorf("expected updated email, got %q", user.Email)
		}
		if user.Name != "Alice Updated" {
			t.Errorf("expected updated name, got %q", user.Name)
		}
		if user.Role != "admin" {
			t.Errorf("expected preserved role admin, got %q", user.Role)
		}

		// ListUsers returns all users.
		db.UpsertUser("sub-2", "bob@example.com", "Bob")
		users, err := db.ListUsers()
		if err != nil {
			t.Fatal(err)
		}
		if len(users) != 2 {
			t.Errorf("expected 2 users, got %d", len(users))
		}

		// UpdateUser changes role and active.
		active := false
		updated, err := db.UpdateUser("sub-2", UserUpdate{Role: strPtr("publisher"), Active: &active})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Role != "publisher" {
			t.Errorf("expected publisher, got %q", updated.Role)
		}
		if updated.Active {
			t.Error("expected inactive after update")
		}

		// UpdateUser for nonexistent user returns nil.
		result, err := db.UpdateUser("nonexistent", UserUpdate{})
		if err != nil {
			t.Fatal(err)
		}
		if result != nil {
			t.Error("expected nil for nonexistent user update")
		}
	})
}

func TestUpsertUserWithRole(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		// UpsertUserWithRole sets a specific role on creation.
		user, err := db.UpsertUserWithRole("admin-sub", "admin@example.com", "Admin", "admin")
		if err != nil {
			t.Fatal(err)
		}
		if user.Role != "admin" {
			t.Errorf("expected admin role, got %q", user.Role)
		}

		// Subsequent UpsertUserWithRole does NOT overwrite the role.
		user, err = db.UpsertUserWithRole("admin-sub", "admin-new@example.com", "Admin New", "viewer")
		if err != nil {
			t.Fatal(err)
		}
		if user.Email != "admin-new@example.com" {
			t.Errorf("expected updated email, got %q", user.Email)
		}
		if user.Role != "admin" {
			t.Errorf("expected preserved role admin, got %q", user.Role)
		}
	})
}

func TestAppAccess(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("access-app", "owner")

		// Grant access.
		if err := db.GrantAppAccess(app.ID, "alice", "user", "viewer", "owner"); err != nil {
			t.Fatal(err)
		}

		// List and verify.
		grants, err := db.ListAppAccess(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(grants) != 1 {
			t.Fatalf("expected 1 grant, got %d", len(grants))
		}
		if grants[0].Principal != "alice" || grants[0].Role != "viewer" {
			t.Errorf("got grant %+v", grants[0])
		}

		// Revoke access.
		revoked, err := db.RevokeAppAccess(app.ID, "alice", "user")
		if err != nil {
			t.Fatal(err)
		}
		if !revoked {
			t.Error("expected revocation")
		}
		grants, _ = db.ListAppAccess(app.ID)
		if len(grants) != 0 {
			t.Errorf("expected 0 grants after revoke, got %d", len(grants))
		}
	})
}

func TestTags(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		// Create tags.
		tag1, err := db.CreateTag("production")
		if err != nil {
			t.Fatal(err)
		}
		tag2, err := db.CreateTag("staging")
		if err != nil {
			t.Fatal(err)
		}

		// List tags.
		tags, err := db.ListTags()
		if err != nil {
			t.Fatal(err)
		}
		if len(tags) != 2 {
			t.Fatalf("expected 2 tags, got %d", len(tags))
		}

		// Get tag by ID.
		fetched, err := db.GetTag(tag1.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fetched.Name != "production" {
			t.Errorf("expected production, got %q", fetched.Name)
		}

		// Add app tags.
		app, _ := db.CreateApp("tagged-app", "admin")
		if err := db.AddAppTag(app.ID, tag1.ID); err != nil {
			t.Fatal(err)
		}
		if err := db.AddAppTag(app.ID, tag2.ID); err != nil {
			t.Fatal(err)
		}

		// ListAppTags.
		appTags, err := db.ListAppTags(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(appTags) != 2 {
			t.Errorf("expected 2 app tags, got %d", len(appTags))
		}

		// Remove app tag.
		removed, err := db.RemoveAppTag(app.ID, tag1.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !removed {
			t.Error("expected removal")
		}
		appTags, _ = db.ListAppTags(app.ID)
		if len(appTags) != 1 {
			t.Errorf("expected 1 app tag after removal, got %d", len(appTags))
		}

		// Delete tag.
		deleted, err := db.DeleteTag(tag2.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !deleted {
			t.Error("expected tag deletion")
		}
	})
}

func TestListAccessibleApps(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		// Create apps with different owners and access types.
		_, _ = db.CreateApp("owned-app", "user-1")
		app2, _ := db.CreateApp("public-app", "user-2")
		db.UpdateApp(app2.ID, AppUpdate{AccessType: strPtr("public")})
		db.CreateApp("private-app", "user-2")
		app4, _ := db.CreateApp("granted-app", "user-2")
		db.GrantAppAccess(app4.ID, "user-1", "user", "viewer", "user-2")
		app5, _ := db.CreateApp("logged-in-app", "user-2")
		db.UpdateApp(app5.ID, AppUpdate{AccessType: strPtr("logged_in")})

		// user-1 should see: owned-app, public-app, granted-app, logged-in-app
		apps, err := db.ListAccessibleApps("user-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(apps) != 4 {
			t.Errorf("expected 4 accessible apps for user-1, got %d", len(apps))
		}

		// user-3 should see public-app and logged-in-app.
		apps, err = db.ListAccessibleApps("user-3")
		if err != nil {
			t.Fatal(err)
		}
		if len(apps) != 2 {
			t.Errorf("expected 2 accessible apps for user-3, got %d", len(apps))
		}

		// user-2 should see all their owned apps + public + logged_in (deduplicated).
		apps, err = db.ListAccessibleApps("user-2")
		if err != nil {
			t.Fatal(err)
		}
		if len(apps) != 4 {
			t.Errorf("expected 4 accessible apps for user-2, got %d", len(apps))
		}
	})
}

func TestRevokeAppAccessNonexistent(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		revoked, err := db.RevokeAppAccess("nonexistent-app", "alice", "user")
		if err != nil {
			t.Fatal(err)
		}
		if revoked {
			t.Error("expected false for nonexistent app access")
		}
	})
}

func TestRemoveAppTagNonexistent(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		removed, err := db.RemoveAppTag("nonexistent-app", "nonexistent-tag")
		if err != nil {
			t.Fatal(err)
		}
		if removed {
			t.Error("expected false for nonexistent app tag")
		}
	})
}

func TestActivateBundle(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")
		db.CreateBundle("b-1", app.ID, "", false)

		if err := db.ActivateBundle(app.ID, "b-1"); err != nil {
			t.Fatal(err)
		}

		b, err := db.GetBundle("b-1")
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != "ready" {
			t.Errorf("expected ready, got %q", b.Status)
		}

		fetched, err := db.GetApp(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fetched.ActiveBundle == nil || *fetched.ActiveBundle != "b-1" {
			t.Errorf("expected active bundle b-1, got %v", fetched.ActiveBundle)
		}

		// Nonexistent bundle ID — fails because active_bundle is a FK to bundles(id).
		if err := db.ActivateBundle(app.ID, "nonexistent-bundle"); err == nil {
			t.Fatal("expected error for nonexistent bundle ID")
		}
	})
}

func TestFailStaleBuildsNone(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		n, err := db.FailStaleBuilds()
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("expected 0 stale builds, got %d", n)
		}
	})
}

func TestFailStaleBuildsMultiple(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")

		// Two bundles in "building" state.
		db.Exec(db.rebind(
			`INSERT INTO bundles (id, app_id, status, uploaded_at)
			 VALUES (?, ?, 'building', '2024-01-01T00:00:00Z')`),
			"sb1", app.ID,
		)
		db.Exec(db.rebind(
			`INSERT INTO bundles (id, app_id, status, uploaded_at)
			 VALUES (?, ?, 'building', '2024-01-01T00:00:00Z')`),
			"sb2", app.ID,
		)
		// One bundle in "ready" state — should not be affected.
		db.CreateBundle("sb3", app.ID, "", false)
		db.UpdateBundleStatus("sb3", "ready")

		n, err := db.FailStaleBuilds()
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("expected 2 stale builds marked failed, got %d", n)
		}

		// Verify statuses.
		b1, _ := db.GetBundle("sb1")
		if b1.Status != "failed" {
			t.Errorf("expected sb1 failed, got %q", b1.Status)
		}
		b2, _ := db.GetBundle("sb2")
		if b2.Status != "failed" {
			t.Errorf("expected sb2 failed, got %q", b2.Status)
		}
		b3, _ := db.GetBundle("sb3")
		if b3.Status != "ready" {
			t.Errorf("expected sb3 still ready, got %q", b3.Status)
		}
	})
}

func TestPATCRUD(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		// Create a user.
		user, err := db.UpsertUser("pat-user", "pat@example.com", "PAT User")
		if err != nil {
			t.Fatal(err)
		}

		// Create a PAT.
		hash := hashPAT("by_testtoken123")
		pat, err := db.CreatePAT("pat-1", hash, user.Sub, "test-token", nil)
		if err != nil {
			t.Fatal(err)
		}
		if pat.ID != "pat-1" {
			t.Errorf("expected pat-1, got %q", pat.ID)
		}

		// LookupPATByHash — should find it and populate User fields.
		result, err := db.LookupPATByHash(hash)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil {
			t.Fatal("expected non-nil lookup result")
			return
		}
		if result.PAT.ID != "pat-1" {
			t.Errorf("expected PAT ID pat-1, got %q", result.PAT.ID)
		}
		if result.User.Sub != "pat-user" {
			t.Errorf("expected user sub pat-user, got %q", result.User.Sub)
		}

		// LookupPATByHash with wrong hash — returns nil.
		wrongHash := hashPAT("by_wrongtoken999")
		missing, err := db.LookupPATByHash(wrongHash)
		if err != nil {
			t.Fatal(err)
		}
		if missing != nil {
			t.Error("expected nil for wrong hash")
		}

		// ListPATsByUser — returns 1 PAT.
		pats, err := db.ListPATsByUser(user.Sub)
		if err != nil {
			t.Fatal(err)
		}
		if len(pats) != 1 {
			t.Errorf("expected 1 PAT, got %d", len(pats))
		}

		// RevokePAT — returns true.
		revoked, err := db.RevokePAT("pat-1", user.Sub)
		if err != nil {
			t.Fatal(err)
		}
		if !revoked {
			t.Error("expected revocation to return true")
		}

		// RevokePAT again — still returns true because the UPDATE WHERE
		// clause matches the row (it does not filter on revoked = 0).
		revoked, err = db.RevokePAT("pat-1", user.Sub)
		if err != nil {
			t.Fatal(err)
		}
		if !revoked {
			t.Error("expected true because row is still matched by WHERE clause")
		}

		// RevokeAllPATs — create 2 more, revoke all, verify count.
		hash2 := hashPAT("by_testtoken456")
		hash3 := hashPAT("by_testtoken789")
		db.CreatePAT("pat-2", hash2, user.Sub, "token-2", nil)
		db.CreatePAT("pat-3", hash3, user.Sub, "token-3", nil)

		count, err := db.RevokeAllPATs(user.Sub)
		if err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Errorf("expected 2 PATs revoked, got %d", count)
		}
	})
}

func TestUpdatePATLastUsed(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		user, err := db.UpsertUser("pat-lu-user", "lu@example.com", "LU User")
		if err != nil {
			t.Fatal(err)
		}
		hash := hashPAT("by_lastusedtest")
		_, err = db.CreatePAT("pat-lu", hash, user.Sub, "last-used-token", nil)
		if err != nil {
			t.Fatal(err)
		}

		ctx := context.Background()
		db.UpdatePATLastUsed(ctx, "pat-lu")

		pats, err := db.ListPATsByUser(user.Sub)
		if err != nil {
			t.Fatal(err)
		}
		if len(pats) == 0 {
			t.Fatal("expected at least one PAT")
		}
		if pats[0].LastUsedAt == nil {
			t.Error("expected last_used_at to be set after UpdatePATLastUsed")
		}
	})
}

func TestCreateBundleForeignKeyViolation(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		_, err := db.CreateBundle("b-orphan", "nonexistent-app-id", "", false)
		if err == nil {
			t.Error("expected error for foreign key violation")
		}
	})
}

func TestCloseRemovesTempFile(t *testing.T) {
	db, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}

	if db.tempPath == "" {
		t.Fatal("expected non-empty tempPath for :memory: DB")
	}
	path := db.tempPath

	// Temp file should exist before Close.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected temp file to exist: %v", err)
	}

	db.Close()

	// Temp file should be removed after Close.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected temp file to be removed, got err: %v", err)
	}
}

func TestOpenUnsupportedDriver(t *testing.T) {
	_, err := Open(config.DatabaseConfig{Driver: "banana", Path: "test.db"})
	if err == nil {
		t.Fatal("expected error for unsupported driver")
	}
}

// --- Soft-delete tests ---

func TestSoftDeleteApp(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")

		if err := db.SoftDeleteApp(app.ID); err != nil {
			t.Fatal(err)
		}

		// App should not be visible via GetApp.
		fetched, _ := db.GetApp(app.ID)
		if fetched != nil {
			t.Error("expected nil from GetApp after soft delete")
		}

		// App should be visible via GetAppIncludeDeleted.
		fetched, _ = db.GetAppIncludeDeleted(app.ID)
		if fetched == nil {
			t.Fatal("expected non-nil from GetAppIncludeDeleted")
			return
		}
		if fetched.DeletedAt == nil {
			t.Error("expected deleted_at to be set")
		}
	})
}

func TestSoftDeleteAppIdempotent(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")
		db.SoftDeleteApp(app.ID)

		// Second soft-delete should be a no-op (WHERE deleted_at IS NULL).
		if err := db.SoftDeleteApp(app.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRestoreApp(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")
		db.SoftDeleteApp(app.ID)

		if err := db.RestoreApp(app.ID); err != nil {
			t.Fatal(err)
		}

		fetched, _ := db.GetApp(app.ID)
		if fetched == nil {
			t.Fatal("expected app to reappear after restore")
			return
		}
		if fetched.DeletedAt != nil {
			t.Error("expected deleted_at to be nil after restore")
		}
	})
}

func TestRestoreNonDeletedApp(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")

		// Restore on a non-deleted app is a no-op.
		if err := db.RestoreApp(app.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestListAppsExcludesDeleted(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		db.CreateApp("app-a", "admin")
		app2, _ := db.CreateApp("app-b", "admin")
		db.SoftDeleteApp(app2.ID)

		apps, _ := db.ListApps()
		if len(apps) != 1 {
			t.Fatalf("expected 1 app, got %d", len(apps))
		}
		if apps[0].Name != "app-a" {
			t.Errorf("expected app-a, got %s", apps[0].Name)
		}
	})
}

func TestListDeletedApps(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app1, _ := db.CreateApp("app-a", "admin")
		db.CreateApp("app-b", "admin")
		db.SoftDeleteApp(app1.ID)

		deleted, _ := db.ListDeletedApps()
		if len(deleted) != 1 {
			t.Fatalf("expected 1 deleted app, got %d", len(deleted))
		}
		if deleted[0].ID != app1.ID {
			t.Error("expected deleted app to be app-a")
		}
	})
}

func TestListExpiredDeletedApps(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app1, _ := db.CreateApp("app-a", "admin")
		db.SoftDeleteApp(app1.ID)

		// Use a cutoff in the future — all soft-deleted apps are expired.
		future := "2099-01-01T00:00:00Z"
		expired, _ := db.ListExpiredDeletedApps(future)
		if len(expired) != 1 {
			t.Fatalf("expected 1 expired app, got %d", len(expired))
		}

		// Use a cutoff in the past — no apps are expired.
		past := "2000-01-01T00:00:00Z"
		expired, _ = db.ListExpiredDeletedApps(past)
		if len(expired) != 0 {
			t.Fatalf("expected 0 expired apps, got %d", len(expired))
		}
	})
}

func TestListCatalogExcludesDeleted(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app1, _ := db.CreateApp("app-a", "admin")
		db.CreateApp("app-b", "admin")
		db.SoftDeleteApp(app1.ID)

		apps, total, err := db.ListCatalog(CatalogParams{
			CallerRole: "admin",
			Page:       1,
			PerPage:    10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			t.Errorf("expected total 1, got %d", total)
		}
		if len(apps) != 1 || apps[0].Name != "app-b" {
			t.Errorf("expected app-b, got %v", apps)
		}
	})
}

func TestListAccessibleAppsExcludesDeleted(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app1, _ := db.CreateApp("app-a", "admin")
		db.CreateApp("app-b", "admin")
		db.SoftDeleteApp(app1.ID)

		apps, _ := db.ListAccessibleApps("admin")
		if len(apps) != 1 {
			t.Fatalf("expected 1 accessible app, got %d", len(apps))
		}
	})
}

func TestHardDeleteApp(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")
		if err := db.HardDeleteApp(app.ID); err != nil {
			t.Fatal(err)
		}
		fetched, _ := db.GetAppIncludeDeleted(app.ID)
		if fetched != nil {
			t.Error("expected nil after hard delete")
		}
	})
}

func TestSoftDeletedNameReusable(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")
		db.SoftDeleteApp(app.ID)

		// Creating a new app with the same name should succeed.
		app2, err := db.CreateApp("my-app", "admin")
		if err != nil {
			t.Fatalf("expected name reuse to succeed: %v", err)
		}
		if app2.Name != "my-app" {
			t.Error("expected new app to have the same name")
		}
	})
}

func TestRestoreWithNameCollision(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app1, _ := db.CreateApp("my-app", "admin")
		db.SoftDeleteApp(app1.ID)

		// Create a new app with the same name.
		db.CreateApp("my-app", "admin")

		// Restoring the original should fail with unique constraint error.
		err := db.RestoreApp(app1.ID)
		if err == nil {
			t.Fatal("expected unique constraint error on restore")
		}
		if !IsUniqueConstraintError(err) {
			t.Fatalf("expected unique constraint error, got: %v", err)
		}
	})
}

// --- Pre-warming tests ---

func TestCreateAppDefaultPreWarmedSeats(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, err := db.CreateApp("warm-app", "admin")
		if err != nil {
			t.Fatal(err)
		}
		if app.PreWarmedSeats != 0 {
			t.Errorf("expected pre_warmed_seats=0, got %d", app.PreWarmedSeats)
		}
	})
}

func TestUpdateAppPreWarmedSeats(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("warm-app", "admin")

		seats := 2
		updated, err := db.UpdateApp(app.ID, AppUpdate{
			PreWarmedSeats: &seats,
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.PreWarmedSeats != 2 {
			t.Errorf("expected pre_warmed_seats=2, got %d", updated.PreWarmedSeats)
		}
	})
}

func TestListPreWarmedApps(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app1, _ := db.CreateApp("warm-app", "admin")
		db.CreateApp("cold-app", "admin")

		seats := 1
		db.UpdateApp(app1.ID, AppUpdate{PreWarmedSeats: &seats})

		apps, err := db.ListPreWarmedApps()
		if err != nil {
			t.Fatal(err)
		}
		if len(apps) != 1 {
			t.Fatalf("expected 1 pre-warmed app, got %d", len(apps))
		}
		if apps[0].ID != app1.ID {
			t.Errorf("expected app %s, got %s", app1.ID, apps[0].ID)
		}
	})
}

func TestListPreWarmedAppsExcludesDeleted(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("warm-app", "admin")
		seats := 1
		db.UpdateApp(app.ID, AppUpdate{PreWarmedSeats: &seats})
		db.SoftDeleteApp(app.ID)

		apps, err := db.ListPreWarmedApps()
		if err != nil {
			t.Fatal(err)
		}
		if len(apps) != 0 {
			t.Errorf("expected 0 pre-warmed apps (deleted excluded), got %d", len(apps))
		}
	})
}

func TestGetAppByNameExcludesDeleted(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")
		db.SoftDeleteApp(app.ID)

		fetched, _ := db.GetAppByName("my-app")
		if fetched != nil {
			t.Error("expected nil from GetAppByName for soft-deleted app")
		}
	})
}

// --- Session CRUD tests ---

func TestSessionCRUD(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("sess-app", "admin")

		// CreateSession
		err := db.CreateSession("s1", app.ID, "w1", "user-a")
		if err != nil {
			t.Fatal(err)
		}

		// GetSession
		s, err := db.GetSession("s1")
		if err != nil {
			t.Fatal(err)
		}
		if s == nil {
			t.Fatal("expected session, got nil")
			return
		}
		if s.AppID != app.ID {
			t.Errorf("expected app_id=%s, got %s", app.ID, s.AppID)
		}
		if s.WorkerID != "w1" {
			t.Errorf("expected worker_id=w1, got %s", s.WorkerID)
		}
		if s.Status != "active" {
			t.Errorf("expected status=active, got %s", s.Status)
		}
		if s.UserSub == nil || *s.UserSub != "user-a" {
			t.Errorf("expected user_sub=user-a, got %v", s.UserSub)
		}

		// GetSession nonexistent
		missing, err := db.GetSession("nonexistent")
		if err != nil {
			t.Fatal(err)
		}
		if missing != nil {
			t.Error("expected nil for nonexistent session")
		}

		// CreateSession without user_sub
		err = db.CreateSession("s2", app.ID, "w1", "")
		if err != nil {
			t.Fatal(err)
		}
		s2, _ := db.GetSession("s2")
		if s2.UserSub != nil {
			t.Errorf("expected nil user_sub, got %v", s2.UserSub)
		}

		// EndSession
		err = db.EndSession("s1", "ended")
		if err != nil {
			t.Fatal(err)
		}
		s, _ = db.GetSession("s1")
		if s.Status != "ended" {
			t.Errorf("expected status=ended, got %s", s.Status)
		}
		if s.EndedAt == nil {
			t.Error("expected ended_at to be set")
		}
	})
}

func TestCrashWorkerSessions(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("crash-app", "admin")
		db.CreateSession("c1", app.ID, "w-crash", "user-a")
		db.CreateSession("c2", app.ID, "w-crash", "user-b")
		db.CreateSession("c3", app.ID, "w-other", "user-c")

		err := db.CrashWorkerSessions("w-crash")
		if err != nil {
			t.Fatal(err)
		}

		s1, _ := db.GetSession("c1")
		s2, _ := db.GetSession("c2")
		s3, _ := db.GetSession("c3")

		if s1.Status != "crashed" {
			t.Errorf("expected c1 crashed, got %s", s1.Status)
		}
		if s2.Status != "crashed" {
			t.Errorf("expected c2 crashed, got %s", s2.Status)
		}
		if s3.Status != "active" {
			t.Errorf("expected c3 active, got %s", s3.Status)
		}
	})
}

func TestEndWorkerSessions(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("end-w-app", "admin")
		db.CreateSession("e1", app.ID, "w-end", "user-a")
		db.CreateSession("e2", app.ID, "w-end", "user-b")

		err := db.EndWorkerSessions("w-end")
		if err != nil {
			t.Fatal(err)
		}

		s1, _ := db.GetSession("e1")
		s2, _ := db.GetSession("e2")
		if s1.Status != "ended" {
			t.Errorf("expected e1 ended, got %s", s1.Status)
		}
		if s2.Status != "ended" {
			t.Errorf("expected e2 ended, got %s", s2.Status)
		}
	})
}

func TestEndAppSessions(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("end-a-app", "admin")
		app2, _ := db.CreateApp("end-a-other", "admin")
		db.CreateSession("a1", app.ID, "w1", "user-a")
		db.CreateSession("a2", app.ID, "w2", "user-b")
		db.CreateSession("a3", app2.ID, "w3", "user-c")

		err := db.EndAppSessions(app.ID)
		if err != nil {
			t.Fatal(err)
		}

		s1, _ := db.GetSession("a1")
		s2, _ := db.GetSession("a2")
		s3, _ := db.GetSession("a3")
		if s1.Status != "ended" {
			t.Errorf("expected a1 ended, got %s", s1.Status)
		}
		if s2.Status != "ended" {
			t.Errorf("expected a2 ended, got %s", s2.Status)
		}
		if s3.Status != "active" {
			t.Errorf("expected a3 active (different app), got %s", s3.Status)
		}
	})
}

func TestListSessions(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("list-sess-app", "admin")
		db.CreateSession("ls1", app.ID, "w1", "user-a")
		db.CreateSession("ls2", app.ID, "w1", "user-b")
		db.CreateSession("ls3", app.ID, "w1", "user-a")

		// List all sessions for the app.
		sessions, err := db.ListSessions(app.ID, SessionListOpts{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != 3 {
			t.Errorf("expected 3 sessions, got %d", len(sessions))
		}

		// Filter by user.
		sessions, err = db.ListSessions(app.ID, SessionListOpts{UserSub: "user-a", Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != 2 {
			t.Errorf("expected 2 sessions for user-a, got %d", len(sessions))
		}

		// Filter by status.
		db.EndSession("ls1", "ended")
		sessions, err = db.ListSessions(app.ID, SessionListOpts{Status: "active", Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != 2 {
			t.Errorf("expected 2 active sessions, got %d", len(sessions))
		}
	})
}

// --- Activity metrics tests ---

func TestCountSessions(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("count-app", "admin")
		db.CreateSession("cnt1", app.ID, "w1", "user-a")
		db.CreateSession("cnt2", app.ID, "w1", "user-b")

		n, err := db.CountSessions(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("expected 2, got %d", n)
		}
	})
}

func TestCountRecentSessions(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("recent-app", "admin")
		db.CreateSession("r1", app.ID, "w1", "user-a")
		db.CreateSession("r2", app.ID, "w1", "user-b")

		// All sessions are recent (just created).
		n, err := db.CountRecentSessions(app.ID, time.Now().AddDate(0, 0, -7))
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("expected 2 recent, got %d", n)
		}

		// Far future since — should return 0.
		n, err = db.CountRecentSessions(app.ID, time.Now().AddDate(1, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("expected 0 from future since, got %d", n)
		}
	})
}

func TestCountUniqueVisitors(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("visitors-app", "admin")
		db.CreateSession("v1", app.ID, "w1", "user-a")
		db.CreateSession("v2", app.ID, "w1", "user-b")
		db.CreateSession("v3", app.ID, "w1", "user-a") // duplicate user
		db.CreateSession("v4", app.ID, "w1", "")        // anonymous

		n, err := db.CountUniqueVisitors(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("expected 2 unique visitors, got %d", n)
		}
	})
}

// --- SetAppEnabled tests ---

func TestSetAppEnabled(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("enable-app", "admin")

		// Default is enabled=true.
		if !app.Enabled {
			t.Error("expected new app to be enabled by default")
		}

		// Disable.
		err := db.SetAppEnabled(app.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		fetched, _ := db.GetApp(app.ID)
		if fetched.Enabled {
			t.Error("expected disabled after SetAppEnabled(false)")
		}

		// Re-enable.
		err = db.SetAppEnabled(app.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		fetched, _ = db.GetApp(app.ID)
		if !fetched.Enabled {
			t.Error("expected enabled after SetAppEnabled(true)")
		}
	})
}

// --- PurgeApp tests ---

func TestPurgeApp(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("purge-app", "admin")
		db.CreateBundle("pb1", app.ID, "", false)
		db.SetActiveBundle(app.ID, "pb1")
		db.CreateSession("ps1", app.ID, "w1", "user-a")
		tag, _ := db.CreateTag("purge-tag")
		db.AddAppTag(app.ID, tag.ID)
		db.GrantAppAccess(app.ID, "alice", "user", "viewer", "admin")

		// Soft-delete first (as the real flow would).
		db.SoftDeleteApp(app.ID)

		// Purge.
		err := db.PurgeApp(app.ID)
		if err != nil {
			t.Fatal(err)
		}

		// App should be completely gone.
		fetched, _ := db.GetApp(app.ID)
		if fetched != nil {
			t.Error("expected nil after purge")
		}
		fetched, _ = db.GetAppIncludeDeleted(app.ID)
		if fetched != nil {
			t.Error("expected nil from GetAppIncludeDeleted after purge")
		}

		// Sessions should be gone.
		s, _ := db.GetSession("ps1")
		if s != nil {
			t.Error("expected session to be purged")
		}

		// Bundle should be gone.
		b, _ := db.GetBundle("pb1")
		if b != nil {
			t.Error("expected bundle to be purged")
		}
	})
}

// --- ListDeployments tests ---

func TestListDeployments(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("deploy-app", "admin")
		db.CreateBundle("d1", app.ID, "", false)
		// Simulate a deployed bundle by setting deployed_at.
		now := time.Now().UTC().Format(time.RFC3339)
		db.Exec(db.rebind(`UPDATE bundles SET deployed_at = ?, deployed_by = ?, status = 'active' WHERE id = ?`),
			now, "admin", "d1")

		// Admin listing.
		rows, total, err := db.ListDeployments(DeploymentListOpts{
			CallerSub:  "admin",
			CallerRole: "admin",
			Page:       1,
			PerPage:    25,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			t.Errorf("expected total=1, got %d", total)
		}
		if len(rows) != 1 {
			t.Errorf("expected 1 deployment, got %d", len(rows))
		}
		if len(rows) > 0 && rows[0].BundleID != "d1" {
			t.Errorf("expected bundle_id=d1, got %s", rows[0].BundleID)
		}
	})
}

func TestListDeploymentsNonAdmin(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("deploy-owner-app", "user-1")
		db.CreateBundle("d2", app.ID, "", false)
		now := time.Now().UTC().Format(time.RFC3339)
		db.Exec(db.rebind(`UPDATE bundles SET deployed_at = ?, status = 'active' WHERE id = ?`),
			now, "d2")

		// Non-admin owner can see own app deployments.
		rows, total, err := db.ListDeployments(DeploymentListOpts{
			CallerSub:  "user-1",
			CallerRole: "publisher",
			Page:       1,
			PerPage:    25,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			t.Errorf("expected total=1, got %d", total)
		}
		if len(rows) != 1 {
			t.Errorf("expected 1 deployment, got %d", len(rows))
		}

		// Different user with no access sees nothing.
		rows, total, err = db.ListDeployments(DeploymentListOpts{
			CallerSub:  "user-2",
			CallerRole: "publisher",
			Page:       1,
			PerPage:    25,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 0 {
			t.Errorf("expected total=0, got %d", total)
		}
		if len(rows) != 0 {
			t.Errorf("expected 0 deployments, got %d", len(rows))
		}
	})
}

// --- ListCatalogWithRelation tests ---

func TestListCatalogWithRelationAdmin(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		db.CreateApp("cwr-a", "user-1")
		db.CreateApp("cwr-b", "user-2")

		rows, total, err := db.ListCatalogWithRelation(CatalogParams{
			CallerRole: "admin",
			CallerSub:  "admin-sub",
			Page:       1,
			PerPage:    25,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 2 {
			t.Errorf("expected total=2, got %d", total)
		}
		if len(rows) != 2 {
			t.Errorf("expected 2 rows, got %d", len(rows))
		}
		// Admin relation.
		for _, r := range rows {
			if r.Relation != "admin" {
				t.Errorf("expected relation=admin, got %s", r.Relation)
			}
		}
	})
}

func TestListCatalogWithRelationOwner(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		db.CreateApp("cwr-own", "user-1")
		db.CreateApp("cwr-other", "user-2")

		rows, total, err := db.ListCatalogWithRelation(CatalogParams{
			CallerSub: "user-1",
			Page:      1,
			PerPage:   25,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			t.Errorf("expected total=1, got %d", total)
		}
		if len(rows) != 1 {
			t.Errorf("expected 1 row, got %d", len(rows))
		}
		if len(rows) > 0 && rows[0].Relation != "owner" {
			t.Errorf("expected relation=owner, got %s", rows[0].Relation)
		}
	})
}

func TestListCatalogWithRelationTags(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("cwr-tagged", "admin")
		tag, _ := db.CreateTag("prod")
		db.AddAppTag(app.ID, tag.ID)

		rows, _, err := db.ListCatalogWithRelation(CatalogParams{
			CallerRole: "admin",
			Page:       1,
			PerPage:    25,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		if rows[0].Tags != "prod" {
			t.Errorf("expected tags=prod, got %q", rows[0].Tags)
		}
	})
}

func TestListCatalogWithRelationSearch(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		db.CreateApp("dashboard-app", "admin")
		db.CreateApp("plumber-app", "admin")

		rows, total, err := db.ListCatalogWithRelation(CatalogParams{
			CallerRole: "admin",
			Search:     "dashboard",
			Page:       1,
			PerPage:    25,
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			t.Errorf("expected total=1, got %d", total)
		}
		if len(rows) != 1 {
			t.Errorf("expected 1 row, got %d", len(rows))
		}
	})
}

// --- ListAppAccessWithNames tests ---

func TestListAppAccessWithNames(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("access-names-app", "owner")
		db.UpsertUser("alice-sub", "alice@example.com", "Alice Smith")
		db.GrantAppAccess(app.ID, "alice-sub", "user", "collaborator", "owner")

		grants, err := db.ListAppAccessWithNames(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(grants) != 1 {
			t.Fatalf("expected 1 grant, got %d", len(grants))
		}
		if grants[0].DisplayName != "Alice Smith" {
			t.Errorf("expected display_name=Alice Smith, got %s", grants[0].DisplayName)
		}
		if grants[0].Role != "collaborator" {
			t.Errorf("expected role=collaborator, got %s", grants[0].Role)
		}
	})
}

func TestListAppAccessWithNamesNoUser(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("access-nouser-app", "owner")
		// Grant to a principal with no user row — should fall back to principal.
		db.GrantAppAccess(app.ID, "unknown-sub", "user", "viewer", "owner")

		grants, err := db.ListAppAccessWithNames(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(grants) != 1 {
			t.Fatalf("expected 1 grant, got %d", len(grants))
		}
		if grants[0].DisplayName != "unknown-sub" {
			t.Errorf("expected display_name=unknown-sub (fallback), got %s", grants[0].DisplayName)
		}
	})
}

// --- GetAppByNameIncludeDeleted tests ---

func TestGetAppByNameIncludeDeleted(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("incl-del", "admin")
		db.SoftDeleteApp(app.ID)

		// Normal lookup should miss it.
		fetched, _ := db.GetAppByName("incl-del")
		if fetched != nil {
			t.Error("expected nil from GetAppByName for deleted app")
		}

		// IncludeDeleted should find it.
		fetched, err := db.GetAppByNameIncludeDeleted("incl-del")
		if err != nil {
			t.Fatal(err)
		}
		if fetched == nil {
			t.Fatal("expected non-nil from GetAppByNameIncludeDeleted")
			return
		}
		if fetched.ID != app.ID {
			t.Errorf("expected id=%s, got %s", app.ID, fetched.ID)
		}

		// Nonexistent.
		missing, err := db.GetAppByNameIncludeDeleted("nonexistent")
		if err != nil {
			t.Fatal(err)
		}
		if missing != nil {
			t.Error("expected nil for nonexistent app")
		}
	})
}

// --- GetAppIncludeDeleted tests ---

func TestGetAppIncludeDeleted(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("incl-del-id", "admin")
		db.SoftDeleteApp(app.ID)

		// Normal lookup should miss it.
		fetched, _ := db.GetApp(app.ID)
		if fetched != nil {
			t.Error("expected nil from GetApp for deleted app")
		}

		// IncludeDeleted should find it.
		fetched, err := db.GetAppIncludeDeleted(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fetched == nil {
			t.Fatal("expected non-nil from GetAppIncludeDeleted")
			return
		}
		if fetched.DeletedAt == nil {
			t.Error("expected deleted_at to be set")
		}
	})
}

// --- RenameApp tests ---

func TestRenameAppSuccess(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("old-name", "admin")

		err := db.RenameApp(app.ID, "old-name", "new-name")
		if err != nil {
			t.Fatal(err)
		}

		// App should have the new name.
		fetched, err := db.GetApp(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fetched.Name != "new-name" {
			t.Errorf("expected new-name, got %q", fetched.Name)
		}

		// Old name should exist as an alias.
		aliasApp, phase, err := db.GetAppByAlias("old-name")
		if err != nil {
			t.Fatal(err)
		}
		if aliasApp == nil {
			t.Fatal("expected alias to resolve to the app")
			return
		}
		if aliasApp.ID != app.ID {
			t.Errorf("expected alias to point to %s, got %s", app.ID, aliasApp.ID)
		}
		if phase != "alias" {
			t.Errorf("expected phase=alias, got %q", phase)
		}
	})
}

func TestRenameAppConflict(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app1, _ := db.CreateApp("app-one", "admin")
		db.CreateApp("app-two", "admin")

		// Renaming app-one to app-two should fail (name already in use).
		err := db.RenameApp(app1.ID, "app-one", "app-two")
		if err == nil {
			t.Fatal("expected error for name conflict")
		}
		if !strings.Contains(err.Error(), "already in use") {
			t.Errorf("expected 'already in use' error, got: %v", err)
		}
	})
}

func TestRenameAppAliasConflict(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app1, _ := db.CreateApp("first", "admin")
		app2, _ := db.CreateApp("second", "admin")

		// Rename first -> renamed-first, creating alias "first".
		err := db.RenameApp(app1.ID, "first", "renamed-first")
		if err != nil {
			t.Fatal(err)
		}

		// Renaming second -> first should fail because "first" is an active alias.
		err = db.RenameApp(app2.ID, "second", "first")
		if err == nil {
			t.Fatal("expected error for alias conflict")
		}
		if !strings.Contains(err.Error(), "reserved as an alias") {
			t.Errorf("expected 'reserved as an alias' error, got: %v", err)
		}
	})
}

// --- GetAppByAlias tests ---

func TestGetAppByAlias(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("original", "admin")
		db.RenameApp(app.ID, "original", "renamed")

		// Alias lookup should resolve to the app.
		fetched, phase, err := db.GetAppByAlias("original")
		if err != nil {
			t.Fatal(err)
		}
		if fetched == nil {
			t.Fatal("expected app from alias lookup")
			return
		}
		if fetched.ID != app.ID {
			t.Errorf("expected app ID %s, got %s", app.ID, fetched.ID)
		}
		if phase != "alias" {
			t.Errorf("expected phase=alias, got %q", phase)
		}
	})
}

func TestGetAppByAliasUnknown(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		fetched, phase, err := db.GetAppByAlias("nonexistent-alias")
		if err != nil {
			t.Fatal(err)
		}
		if fetched != nil {
			t.Error("expected nil for unknown alias")
		}
		if phase != "" {
			t.Errorf("expected empty phase, got %q", phase)
		}
	})
}

// --- TransitionExpiredAliases tests ---

func TestTransitionExpiredAliases(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("trans-app", "admin")

		// Insert an alias that expired in the past.
		pastExpiry := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
		_, err := db.Exec(db.rebind(
			`INSERT INTO app_aliases (app_id, name, phase, expires_at)
			 VALUES (?, ?, 'alias', ?)`),
			app.ID, "old-alias", pastExpiry)
		if err != nil {
			t.Fatal(err)
		}

		// Insert an alias that has not expired.
		futureExpiry := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
		_, err = db.Exec(db.rebind(
			`INSERT INTO app_aliases (app_id, name, phase, expires_at)
			 VALUES (?, ?, 'alias', ?)`),
			app.ID, "fresh-alias", futureExpiry)
		if err != nil {
			t.Fatal(err)
		}

		if err := db.TransitionExpiredAliases(); err != nil {
			t.Fatal(err)
		}

		// Expired alias should now be in redirect phase.
		_, phase, err := db.GetAppByAlias("old-alias")
		if err != nil {
			t.Fatal(err)
		}
		if phase != "redirect" {
			t.Errorf("expected phase=redirect for expired alias, got %q", phase)
		}

		// Fresh alias should remain in alias phase.
		_, phase, err = db.GetAppByAlias("fresh-alias")
		if err != nil {
			t.Fatal(err)
		}
		if phase != "alias" {
			t.Errorf("expected phase=alias for fresh alias, got %q", phase)
		}
	})
}

// --- CleanupExpiredRedirects tests ---

func TestCleanupExpiredRedirects(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("cleanup-app", "admin")

		// Insert an expired redirect.
		pastExpiry := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
		_, err := db.Exec(db.rebind(
			`INSERT INTO app_aliases (app_id, name, phase, expires_at)
			 VALUES (?, ?, 'redirect', ?)`),
			app.ID, "expired-redirect", pastExpiry)
		if err != nil {
			t.Fatal(err)
		}

		// Insert a non-expired redirect.
		futureExpiry := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
		_, err = db.Exec(db.rebind(
			`INSERT INTO app_aliases (app_id, name, phase, expires_at)
			 VALUES (?, ?, 'redirect', ?)`),
			app.ID, "valid-redirect", futureExpiry)
		if err != nil {
			t.Fatal(err)
		}

		if err := db.CleanupExpiredRedirects(); err != nil {
			t.Fatal(err)
		}

		// Expired redirect should be gone.
		fetched, _, err := db.GetAppByAlias("expired-redirect")
		if err != nil {
			t.Fatal(err)
		}
		if fetched != nil {
			t.Error("expected expired redirect to be deleted")
		}

		// Non-expired redirect should remain.
		fetched, phase, err := db.GetAppByAlias("valid-redirect")
		if err != nil {
			t.Fatal(err)
		}
		if fetched == nil {
			t.Error("expected non-expired redirect to remain")
		}
		if phase != "redirect" {
			t.Errorf("expected phase=redirect, got %q", phase)
		}
	})
}

// --- BundleLog tests ---

func TestInsertAndGetBundleLog(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("log-app", "admin")
		db.CreateBundle("log-b1", app.ID, "", false)

		logOutput := "Step 1: installing packages\nStep 2: building image\nDone."
		if err := db.InsertBundleLog("log-b1", logOutput); err != nil {
			t.Fatal(err)
		}

		fetched, err := db.GetBundleLog("log-b1")
		if err != nil {
			t.Fatal(err)
		}
		if fetched != logOutput {
			t.Errorf("expected log output %q, got %q", logOutput, fetched)
		}
	})
}

func TestGetBundleLogMissing(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		output, err := db.GetBundleLog("nonexistent-bundle")
		if err != nil {
			t.Fatal(err)
		}
		if output != "" {
			t.Errorf("expected empty string for missing bundle log, got %q", output)
		}
	})
}

// --- SearchUsers tests ---

func TestSearchUsers(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		db.UpsertUser("su-1", "alice@example.com", "Alice Smith")
		db.UpsertUser("su-2", "bob@example.com", "Bob Jones")
		db.UpsertUser("su-3", "charlie@example.com", "Charlie Smith")

		// Search by name substring.
		users, err := db.SearchUsers("Smith", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(users) != 2 {
			t.Errorf("expected 2 users matching 'Smith', got %d", len(users))
		}

		// Search by email substring.
		users, err = db.SearchUsers("bob@", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(users) != 1 {
			t.Errorf("expected 1 user matching 'bob@', got %d", len(users))
		}
		if len(users) > 0 && users[0].Sub != "su-2" {
			t.Errorf("expected su-2, got %q", users[0].Sub)
		}

		// Limit controls result count.
		users, err = db.SearchUsers("example.com", 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(users) != 2 {
			t.Errorf("expected 2 users (limited), got %d", len(users))
		}

		// No matches.
		users, err = db.SearchUsers("zzz-no-match", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(users) != 0 {
			t.Errorf("expected 0 users, got %d", len(users))
		}
	})
}

// --- ListTagsWithCounts tests ---

func TestListTagsWithCounts(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		tag1, _ := db.CreateTag("prod")
		tag2, _ := db.CreateTag("staging")
		tag3, _ := db.CreateTag("empty-tag")

		app1, _ := db.CreateApp("twc-app-1", "admin")
		app2, _ := db.CreateApp("twc-app-2", "admin")

		db.AddAppTag(app1.ID, tag1.ID)
		db.AddAppTag(app2.ID, tag1.ID)
		db.AddAppTag(app1.ID, tag2.ID)
		// tag3 has no apps

		tags, err := db.ListTagsWithCounts()
		if err != nil {
			t.Fatal(err)
		}
		if len(tags) != 3 {
			t.Fatalf("expected 3 tags, got %d", len(tags))
		}

		// Build a lookup by name for easy assertions.
		counts := make(map[string]int)
		for _, tg := range tags {
			counts[tg.Name] = tg.AppCount
		}

		if counts["prod"] != 2 {
			t.Errorf("expected prod count=2, got %d", counts["prod"])
		}
		if counts["staging"] != 1 {
			t.Errorf("expected staging count=1, got %d", counts["staging"])
		}
		if counts["empty-tag"] != 0 {
			t.Errorf("expected empty-tag count=0, got %d", counts["empty-tag"])
		}

		// Deleted apps should not count.
		db.SoftDeleteApp(app1.ID)
		tags, err = db.ListTagsWithCounts()
		if err != nil {
			t.Fatal(err)
		}
		counts = make(map[string]int)
		for _, tg := range tags {
			counts[tg.Name] = tg.AppCount
		}
		if counts["prod"] != 1 {
			t.Errorf("after delete: expected prod count=1, got %d", counts["prod"])
		}
		if counts["staging"] != 0 {
			t.Errorf("after delete: expected staging count=0, got %d", counts["staging"])
		}

		_ = tag3 // used above
	})
}

// --- RenameTag tests ---

func TestRenameTag(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		tag, _ := db.CreateTag("old-tag")

		err := db.RenameTag(tag.ID, "new-tag")
		if err != nil {
			t.Fatal(err)
		}

		fetched, err := db.GetTag(tag.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fetched.Name != "new-tag" {
			t.Errorf("expected new-tag, got %q", fetched.Name)
		}
	})
}

func TestRenameTagUniqueConflict(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		tag1, _ := db.CreateTag("tag-alpha")
		db.CreateTag("tag-beta")

		// Renaming tag-alpha to tag-beta should violate unique constraint.
		err := db.RenameTag(tag1.ID, "tag-beta")
		if err == nil {
			t.Fatal("expected unique constraint error")
		}
		if !IsUniqueConstraintError(err) {
			t.Errorf("expected unique constraint error, got: %v", err)
		}
	})
}

func TestUpdateApp_ImageRuntime(t *testing.T) {
	db := testDB(t)

	app, err := db.CreateApp("img-rt-test", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if app.Image != "" {
		t.Errorf("default image should be empty, got %q", app.Image)
	}
	if app.Runtime != "" {
		t.Errorf("default runtime should be empty, got %q", app.Runtime)
	}

	img := "custom:latest"
	rt := "kata-runtime"
	app, err = db.UpdateApp(app.ID, AppUpdate{Image: &img, Runtime: &rt})
	if err != nil {
		t.Fatal(err)
	}
	if app.Image != img {
		t.Errorf("Image = %q, want %q", app.Image, img)
	}
	if app.Runtime != rt {
		t.Errorf("Runtime = %q, want %q", app.Runtime, rt)
	}

	// Clear back to empty.
	empty := ""
	app, err = db.UpdateApp(app.ID, AppUpdate{Image: &empty, Runtime: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if app.Image != "" {
		t.Errorf("Image should be cleared, got %q", app.Image)
	}
	if app.Runtime != "" {
		t.Errorf("Runtime should be cleared, got %q", app.Runtime)
	}
}

func TestSetAppDataMounts(t *testing.T) {
	db := testDB(t)

	app, err := db.CreateApp("mount-test", "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Initially empty.
	mounts, err := db.ListAppDataMounts(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 0 {
		t.Fatalf("expected 0 mounts, got %d", len(mounts))
	}

	// Set mounts.
	err = db.SetAppDataMounts(app.ID, []DataMountRow{
		{AppID: app.ID, Source: "models", Target: "/data/models", ReadOnly: true},
		{AppID: app.ID, Source: "scratch", Target: "/data/scratch", ReadOnly: false},
	})
	if err != nil {
		t.Fatal(err)
	}

	mounts, err = db.ListAppDataMounts(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}

	// Replace with single mount.
	err = db.SetAppDataMounts(app.ID, []DataMountRow{
		{AppID: app.ID, Source: "models/v2", Target: "/data/models", ReadOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	mounts, err = db.ListAppDataMounts(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	if mounts[0].Source != "models/v2" {
		t.Errorf("Source = %q, want %q", mounts[0].Source, "models/v2")
	}

	// Clear all.
	err = db.SetAppDataMounts(app.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	mounts, err = db.ListAppDataMounts(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 0 {
		t.Fatalf("expected 0 mounts, got %d", len(mounts))
	}
}

func TestDataMountsCascadeDelete(t *testing.T) {
	db := testDB(t)

	app, err := db.CreateApp("cascade-test", "admin")
	if err != nil {
		t.Fatal(err)
	}

	err = db.SetAppDataMounts(app.ID, []DataMountRow{
		{AppID: app.ID, Source: "models", Target: "/data/models", ReadOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Soft-delete, then hard-delete to trigger CASCADE.
	if err := db.SoftDeleteApp(app.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.HardDeleteApp(app.ID); err != nil {
		t.Fatal(err)
	}

	// Mounts should be gone.
	mounts, err := db.ListAppDataMounts(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 0 {
		t.Fatalf("expected 0 mounts after cascade delete, got %d", len(mounts))
	}
}
