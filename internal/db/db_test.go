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

func TestCreateAndGetBundle(t *testing.T) {
	db := testDB(t)
	app, _ := db.CreateApp("my-app")

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
	app, _ := db.CreateApp("my-app")

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
	app, _ := db.CreateApp("my-app")
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
	app, _ := db.CreateApp("my-app")
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
	app, _ := db.CreateApp("my-app")
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
	app, _ := db.CreateApp("my-app")

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
	app, _ := db.CreateApp("my-app")
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

	app, _ := db.CreateApp("my-app")

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
