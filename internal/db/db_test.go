package db

import "testing"

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

	app, err := db.CreateApp("my-app")
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

	app, _ := db.CreateApp("my-app")

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

	_, err := db.CreateApp("my-app")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateApp("my-app")
	if err == nil {
		t.Error("expected error on duplicate name")
	}
}

func TestDeleteApp(t *testing.T) {
	db := testDB(t)

	app, _ := db.CreateApp("my-app")
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

	db.CreateApp("app-a")
	db.CreateApp("app-b")

	apps, err := db.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 {
		t.Errorf("expected 2 apps, got %d", len(apps))
	}
}

func TestFailStaleBuilds(t *testing.T) {
	db := testDB(t)

	app, _ := db.CreateApp("my-app")

	// Insert a bundle in "building" state
	_, err := db.Exec(
		`INSERT INTO bundles (id, app_id, status, path, uploaded_at)
		 VALUES ('b1', ?, 'building', '/tmp/b1.tar.gz', '2024-01-01T00:00:00Z')`,
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
