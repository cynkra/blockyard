package db

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/cynkra/blockyard/internal/config"
)

func TestSetBundleDeployed(t *testing.T) {
	db := testDB(t)
	app, err := db.CreateApp("test-app", "admin")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := db.CreateBundle("bundle-1", app.ID, "admin", false)
	if err != nil {
		t.Fatal(err)
	}

	err = db.SetBundleDeployed(bundle.ID, "deployer-user")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := db.GetBundle(bundle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.DeployedBy == nil || *updated.DeployedBy != "deployer-user" {
		t.Errorf("DeployedBy = %v, want %q", updated.DeployedBy, "deployer-user")
	}
	if updated.DeployedAt == nil {
		t.Error("expected DeployedAt to be set")
	}
}

func TestSetBundleDeployedNonExistent(t *testing.T) {
	db := testDB(t)
	// Should not error even for non-existent bundle (UPDATE affects 0 rows).
	err := db.SetBundleDeployed("nonexistent", "admin")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPATHashExists(t *testing.T) {
	db := testDB(t)
	db.UpsertUser("user-1", "user@example.com", "User One")

	hash := sha256.Sum256([]byte("test-token"))
	tokenHash := hash[:]

	// Before creating — should not exist.
	if db.PATHashExists(tokenHash) {
		t.Error("expected PATHashExists=false before creation")
	}

	// Create a PAT.
	_, err := db.CreatePAT("pat-1", tokenHash, "user-1", "test pat", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Now should exist.
	if !db.PATHashExists(tokenHash) {
		t.Error("expected PATHashExists=true after creation")
	}

	// Different hash should not exist.
	otherHash := sha256.Sum256([]byte("other-token"))
	if db.PATHashExists(otherHash[:]) {
		t.Error("expected PATHashExists=false for different hash")
	}
}

func TestBackupSQLiteWithMeta_InMemoryFails(t *testing.T) {
	db := testDB(t)
	_, err := db.BackupWithMeta(context.Background(), "v1.0.0")
	if err == nil {
		t.Error("expected error backing up in-memory database")
	}
}

func TestMigrateDownOneStep(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(testDBConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ver, _, _ := db.MigrationVersion()
	if ver < 2 {
		t.Skip("need at least 2 migrations to test MigrateDown")
	}

	// Migrate down one step.
	target := ver - 1
	if err := db.MigrateDown(target); err != nil {
		t.Fatalf("MigrateDown to %d: %v", target, err)
	}

	newVer, _, _ := db.MigrationVersion()
	if newVer != target {
		t.Errorf("version after MigrateDown = %d, want %d", newVer, target)
	}
}

func testDBConfig(dir string) config.DatabaseConfig {
	return config.DatabaseConfig{
		Driver: "sqlite",
		Path:   dir + "/test.db",
	}
}

func TestListDeploymentsSortVariants(t *testing.T) {
	db := testDB(t)

	db.UpsertUser("admin-sub", "admin@test.com", "Admin")
	app, _ := db.CreateApp("sort-test", "admin-sub")
	bundle, _ := db.CreateBundle("b-sort", app.ID, "admin-sub", false)
	db.SetBundleDeployed(bundle.ID, "admin-sub")

	// Exercise remaining sort columns that existing tests don't cover.
	for _, col := range []string{"deployed_by", "status"} {
		t.Run("sort_"+col, func(t *testing.T) {
			_, _, err := db.ListDeployments(DeploymentListOpts{
				CallerRole: "admin",
				Sort:       col,
				SortDir:    "asc",
				Page:       1,
				PerPage:    10,
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}

	// Status filter.
	t.Run("status filter", func(t *testing.T) {
		_, _, err := db.ListDeployments(DeploymentListOpts{
			CallerRole: "admin",
			Status:     "pending",
			Page:       1,
			PerPage:    10,
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestListCatalogWithRelationTagFilter(t *testing.T) {
	db := testDB(t)

	db.UpsertUser("user-tag", "tag@test.com", "Tag User")
	app, _ := db.CreateApp("tag-app", "user-tag")
	tag, err := db.CreateTag("prod")
	if err != nil {
		t.Fatal(err)
	}
	db.AddAppTag(app.ID, tag.ID)

	// Filter by tag with OR mode.
	rows, _, err := db.ListCatalogWithRelation(CatalogParams{
		CallerRole: "admin",
		Tags:       []string{"prod"},
		TagMode:    "or",
		Page:       1,
		PerPage:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Error("expected tag filter to find the app")
	}

	// Legacy single-tag filter.
	rows, _, err = db.ListCatalogWithRelation(CatalogParams{
		CallerRole: "admin",
		Tag:        "prod",
		Page:       1,
		PerPage:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Error("expected legacy tag filter to find the app")
	}
}
