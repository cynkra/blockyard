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

	"github.com/cynkra/blockyard/internal/config"
	"github.com/google/uuid"
)

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

	baseURL := os.Getenv("BLOCKYARD_TEST_POSTGRES_URL")
	if baseURL == "" {
		t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set; skipping PostgreSQL tests")
	}

	// Create a unique database for this test to avoid cross-test pollution.
	dbName := "test_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]

	// Connect to the default database to create the test database.
	adminDB, err := sql.Open("pgx", baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminDB.Exec("CREATE DATABASE " + dbName); err != nil {
		adminDB.Close()
		t.Fatal(err)
	}
	adminDB.Close()

	// Build the test database URL by replacing the database name.
	testURL := replaceDBName(baseURL, dbName)
	db, err := Open(config.DatabaseConfig{Driver: "postgres", URL: testURL})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		db.Close()
		// Drop the test database.
		cleanup, _ := sql.Open("pgx", baseURL)
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
func eachDB(t *testing.T, fn func(t *testing.T, db *DB)) {
	t.Run("sqlite", func(t *testing.T) {
		fn(t, testDB(t))
	})
	t.Run("postgres", func(t *testing.T) {
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

		b, err := db.CreateBundle("b-1", app.ID)
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

		db.CreateBundle("b-1", app.ID)
		db.CreateBundle("b-2", app.ID)

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
		db.CreateBundle("b-1", app.ID)

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
		db.CreateBundle("b-1", app.ID)

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
		db.CreateBundle("b-1", app.ID)

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
		db.CreateBundle("b-1", app.ID)
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
		db.CreateBundle("b-1", app.ID)

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
		db.CreateBundle("sb3", app.ID)
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

func TestCreateBundleForeignKeyViolation(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		_, err := db.CreateBundle("b-orphan", "nonexistent-app-id")
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
