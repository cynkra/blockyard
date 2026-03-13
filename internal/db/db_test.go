package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreateAndGetApp(t *testing.T) {
	db := testDB(t)

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
}

func TestGetAppByName(t *testing.T) {
	db := testDB(t)

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
}

func TestDuplicateNameFails(t *testing.T) {
	db := testDB(t)

	_, err := db.CreateApp("my-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateApp("my-app", "admin")
	if err == nil {
		t.Error("expected error on duplicate name")
	}
}

func TestDeleteApp(t *testing.T) {
	db := testDB(t)

	app, _ := db.CreateApp("my-app", "admin")
	deleted, err := db.DeleteApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Error("expected deletion")
	}

	fetched, _ := db.GetApp(app.ID)
	if fetched != nil {
		t.Error("expected nil after deletion")
	}
}

func TestListApps(t *testing.T) {
	db := testDB(t)

	db.CreateApp("app-a", "admin")
	db.CreateApp("app-b", "admin")

	apps, err := db.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 {
		t.Errorf("expected 2 apps, got %d", len(apps))
	}
}

func TestCreateAndGetBundle(t *testing.T) {
	db := testDB(t)
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
}

func TestListBundlesByApp(t *testing.T) {
	db := testDB(t)
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
}

func TestUpdateBundleStatus(t *testing.T) {
	db := testDB(t)
	app, _ := db.CreateApp("my-app", "admin")
	db.CreateBundle("b-1", app.ID)

	if err := db.UpdateBundleStatus("b-1", "building"); err != nil {
		t.Fatal(err)
	}

	b, _ := db.GetBundle("b-1")
	if b.Status != "building" {
		t.Errorf("expected building, got %q", b.Status)
	}
}

func TestSetActiveBundle(t *testing.T) {
	db := testDB(t)
	app, _ := db.CreateApp("my-app", "admin")
	db.CreateBundle("b-1", app.ID)

	if err := db.SetActiveBundle(app.ID, "b-1"); err != nil {
		t.Fatal(err)
	}

	fetched, _ := db.GetApp(app.ID)
	if fetched.ActiveBundle == nil || *fetched.ActiveBundle != "b-1" {
		t.Errorf("expected active bundle b-1, got %v", fetched.ActiveBundle)
	}
}

func TestDeleteBundle(t *testing.T) {
	db := testDB(t)
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
}

func TestUpdateApp(t *testing.T) {
	db := testDB(t)
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
}

func TestUpdateAppNotFound(t *testing.T) {
	db := testDB(t)
	_, err := db.UpdateApp("nonexistent", AppUpdate{})
	if err == nil {
		t.Error("expected error for nonexistent app")
	}
}

func TestClearActiveBundle(t *testing.T) {
	db := testDB(t)
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
}

func TestFailStaleBuilds(t *testing.T) {
	db := testDB(t)

	app, _ := db.CreateApp("my-app", "admin")

	// Insert a bundle in "building" state
	_, err := db.Exec(
		`INSERT INTO bundles (id, app_id, status, uploaded_at)
		 VALUES ('b1', ?, 'building', '2024-01-01T00:00:00Z')`,
		app.ID,
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
	var status string
	db.QueryRow(`SELECT status FROM bundles WHERE id = 'b1'`).Scan(&status)
	if status != "failed" {
		t.Errorf("expected 'failed', got %q", status)
	}
}

func TestOpenCreatesDirectory(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "subdir", "nested", "test.db")

	database, err := Open(dbPath)
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
	_, err := Open(dbPath)
	if err == nil {
		t.Fatal("expected error opening DB under a file path")
	}
}

func TestListCatalogAdmin(t *testing.T) {
	database := testDB(t)

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
}

func TestListCatalogUnauthenticated(t *testing.T) {
	database := testDB(t)

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
}

func TestListCatalogOwnerFilter(t *testing.T) {
	database := testDB(t)

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
}

func TestListCatalogSearch(t *testing.T) {
	database := testDB(t)

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
}

func TestListCatalogPagination(t *testing.T) {
	database := testDB(t)

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
}

func TestListCatalogTagFilter(t *testing.T) {
	database := testDB(t)

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
}

func TestListCatalogWithGroups(t *testing.T) {
	database := testDB(t)

	app1, _ := database.CreateApp("group-app", "owner-1")
	database.CreateApp("private-app", "owner-2")

	// Grant group access
	database.GrantAppAccess(app1.ID, "team-a", "group", "viewer", "owner-1")

	apps, total, err := database.ListCatalog(CatalogParams{
		CallerSub:    "user-x",
		CallerGroups: []string{"team-a"},
		Page:         1,
		PerPage:      10,
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
}

func strPtr(s string) *string { return &s }

func TestOpenMemory(t *testing.T) {
	db, err := Open(":memory:")
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
	db := testDB(t)
	ctx := context.Background()

	if err := db.Ping(ctx); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
}

func TestIsUniqueConstraintError(t *testing.T) {
	db := testDB(t)

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
}

func TestGetAppNonexistent(t *testing.T) {
	db := testDB(t)

	app, err := db.GetApp("00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if app != nil {
		t.Error("expected nil for nonexistent app")
	}
}

func TestGetBundleNonexistent(t *testing.T) {
	db := testDB(t)

	bundle, err := db.GetBundle("nonexistent-bundle-id")
	if err != nil {
		t.Fatal(err)
	}
	if bundle != nil {
		t.Error("expected nil for nonexistent bundle")
	}
}

func TestDeleteAppNonexistent(t *testing.T) {
	db := testDB(t)

	deleted, err := db.DeleteApp("00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("expected false for nonexistent app")
	}
}

func TestDeleteBundleNonexistent(t *testing.T) {
	db := testDB(t)

	deleted, err := db.DeleteBundle("nonexistent-bundle-id")
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("expected false for nonexistent bundle")
	}
}

func TestDeleteTagNonexistent(t *testing.T) {
	db := testDB(t)

	deleted, err := db.DeleteTag("nonexistent-tag-id")
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("expected false for nonexistent tag")
	}
}

func TestRoleMappings(t *testing.T) {
	db := testDB(t)

	// Create a role mapping.
	if err := db.UpsertRoleMapping("admins", "admin"); err != nil {
		t.Fatal(err)
	}

	// List and verify.
	mappings, err := db.ListRoleMappings()
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}
	if mappings[0].GroupName != "admins" || mappings[0].Role != "admin" {
		t.Errorf("got mapping %+v", mappings[0])
	}

	// Update (upsert) the role.
	if err := db.UpsertRoleMapping("admins", "viewer"); err != nil {
		t.Fatal(err)
	}
	mappings, _ = db.ListRoleMappings()
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping after upsert, got %d", len(mappings))
	}
	if mappings[0].Role != "viewer" {
		t.Errorf("expected role viewer after upsert, got %q", mappings[0].Role)
	}

	// Delete.
	deleted, err := db.DeleteRoleMapping("admins")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Error("expected deletion")
	}
	mappings, _ = db.ListRoleMappings()
	if len(mappings) != 0 {
		t.Errorf("expected 0 mappings after delete, got %d", len(mappings))
	}
}

func TestAppAccess(t *testing.T) {
	db := testDB(t)

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
}

func TestTags(t *testing.T) {
	db := testDB(t)

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
}

func TestListAccessibleApps(t *testing.T) {
	db := testDB(t)

	// Create apps with different owners and access types.
	app1, _ := db.CreateApp("owned-app", "user-1")
	app2, _ := db.CreateApp("public-app", "user-2")
	db.UpdateApp(app2.ID, AppUpdate{AccessType: strPtr("public")})
	db.CreateApp("private-app", "user-2")
	app4, _ := db.CreateApp("granted-app", "user-2")
	db.GrantAppAccess(app4.ID, "user-1", "user", "viewer", "user-2")
	app5, _ := db.CreateApp("group-app", "user-2")
	db.GrantAppAccess(app5.ID, "team-a", "group", "viewer", "user-2")

	// user-1 should see: owned-app, public-app, granted-app, group-app
	apps, err := db.ListAccessibleApps("user-1", []string{"team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 4 {
		t.Errorf("expected 4 accessible apps for user-1, got %d", len(apps))
	}

	// user-3 with no groups should see only public-app.
	apps, err = db.ListAccessibleApps("user-3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 {
		t.Errorf("expected 1 accessible app for user-3, got %d", len(apps))
	}
	if len(apps) > 0 && apps[0].ID != app2.ID {
		t.Errorf("expected public-app, got %q", apps[0].Name)
	}

	// user-2 should see all their owned apps + public.
	apps, err = db.ListAccessibleApps("user-2", nil)
	if err != nil {
		t.Fatal(err)
	}
	// user-2 owns: public-app, private-app, granted-app, group-app (4 apps)
	// plus public-app is also visible via access_type, but it's already owned.
	if len(apps) != 4 {
		t.Errorf("expected 4 accessible apps for user-2, got %d", len(apps))
	}

	_ = app1 // used above
}

func TestDeleteRoleMappingNonexistent(t *testing.T) {
	db := testDB(t)

	deleted, err := db.DeleteRoleMapping("nonexistent-group")
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("expected false for nonexistent role mapping")
	}
}

func TestRevokeAppAccessNonexistent(t *testing.T) {
	db := testDB(t)

	revoked, err := db.RevokeAppAccess("nonexistent-app", "alice", "user")
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Error("expected false for nonexistent app access")
	}
}

func TestRemoveAppTagNonexistent(t *testing.T) {
	db := testDB(t)

	removed, err := db.RemoveAppTag("nonexistent-app", "nonexistent-tag")
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Error("expected false for nonexistent app tag")
	}
}
