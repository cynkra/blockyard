package db

import (
	"testing"
	"time"
)

// setRefreshSchedule is a test helper that sets refresh_schedule directly via SQL,
// since AppUpdate does not expose this field (it is set during bundle upload).
func setRefreshSchedule(t *testing.T, db *DB, appID, schedule string) {
	t.Helper()
	_, err := db.Exec(db.rebind(`UPDATE apps SET refresh_schedule = ? WHERE id = ?`), schedule, appID)
	if err != nil {
		t.Fatal(err)
	}
}

func TestListAppsWithRefreshSchedule_Empty(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		// Create two apps with no refresh schedule (default empty string).
		db.CreateApp("app-a", "admin")
		db.CreateApp("app-b", "admin")

		apps, err := db.ListAppsWithRefreshSchedule()
		if err != nil {
			t.Fatal(err)
		}
		if len(apps) != 0 {
			t.Errorf("expected 0 apps, got %d", len(apps))
		}
	})
}

func TestListAppsWithRefreshSchedule_ReturnsScheduled(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		a, _ := db.CreateApp("scheduled-app", "admin")
		db.CreateApp("plain-app", "admin")

		setRefreshSchedule(t, db, a.ID, "0 3 * * *")

		apps, err := db.ListAppsWithRefreshSchedule()
		if err != nil {
			t.Fatal(err)
		}
		if len(apps) != 1 {
			t.Fatalf("expected 1 app, got %d", len(apps))
		}
		if apps[0].ID != a.ID {
			t.Errorf("expected app ID %q, got %q", a.ID, apps[0].ID)
		}
		if apps[0].RefreshSchedule != "0 3 * * *" {
			t.Errorf("expected schedule %q, got %q", "0 3 * * *", apps[0].RefreshSchedule)
		}
	})
}

func TestListAppsWithRefreshSchedule_ExcludesDeleted(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		a, _ := db.CreateApp("will-delete", "admin")
		b, _ := db.CreateApp("will-keep", "admin")

		setRefreshSchedule(t, db, a.ID, "0 3 * * *")
		setRefreshSchedule(t, db, b.ID, "0 6 * * *")

		if err := db.SoftDeleteApp(a.ID); err != nil {
			t.Fatal(err)
		}

		apps, err := db.ListAppsWithRefreshSchedule()
		if err != nil {
			t.Fatal(err)
		}
		if len(apps) != 1 {
			t.Fatalf("expected 1 app after soft-delete, got %d", len(apps))
		}
		if apps[0].ID != b.ID {
			t.Errorf("expected app ID %q, got %q", b.ID, apps[0].ID)
		}
	})
}

func TestUpdateLastRefresh(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("my-app", "admin")

		// Before any refresh, last_refresh_at should be nil.
		fetched, err := db.GetApp(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fetched.LastRefreshAt != nil {
			t.Errorf("expected nil LastRefreshAt, got %v", fetched.LastRefreshAt)
		}

		now := time.Date(2026, 3, 26, 12, 0, 0, 0, time.UTC)
		if err := db.UpdateLastRefresh(app.ID, now); err != nil {
			t.Fatal(err)
		}

		fetched, err = db.GetApp(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fetched.LastRefreshAt == nil {
			t.Fatal("expected non-nil LastRefreshAt after update")
		}
		if *fetched.LastRefreshAt != "2026-03-26T12:00:00Z" {
			t.Errorf("expected 2026-03-26T12:00:00Z, got %q", *fetched.LastRefreshAt)
		}
	})
}

func TestUpdateLastRefresh_RoundTrip(t *testing.T) {
	eachDB(t, func(t *testing.T, db *DB) {
		app, _ := db.CreateApp("round-trip-app", "admin")
		setRefreshSchedule(t, db, app.ID, "0 */6 * * *")

		ts := time.Date(2026, 1, 15, 8, 30, 0, 0, time.UTC)
		if err := db.UpdateLastRefresh(app.ID, ts); err != nil {
			t.Fatal(err)
		}

		// Verify via ListAppsWithRefreshSchedule that the timestamp survives
		// the round-trip through the database.
		apps, err := db.ListAppsWithRefreshSchedule()
		if err != nil {
			t.Fatal(err)
		}
		if len(apps) != 1 {
			t.Fatalf("expected 1 scheduled app, got %d", len(apps))
		}
		if apps[0].LastRefreshAt == nil {
			t.Fatal("expected non-nil LastRefreshAt in listed app")
		}

		got, err := time.Parse(time.RFC3339, *apps[0].LastRefreshAt)
		if err != nil {
			t.Fatalf("failed to parse LastRefreshAt %q: %v", *apps[0].LastRefreshAt, err)
		}
		if !got.Equal(ts) {
			t.Errorf("expected %v, got %v", ts, got)
		}
	})
}
