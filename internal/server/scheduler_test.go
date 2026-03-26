package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/manifest"
	"github.com/cynkra/blockyard/internal/task"
)

func TestShouldRun_NeverRunBefore(t *testing.T) {
	// "every minute" schedule, never run before → should fire.
	if !shouldRun("* * * * *", nil, time.Now()) {
		t.Error("expected shouldRun=true when never run before")
	}
}

func TestShouldRun_RecentlyRun(t *testing.T) {
	// "every hour" schedule, last run 30s ago → should not fire.
	last := time.Now().Add(-30 * time.Second).Format(time.RFC3339)
	if shouldRun("0 * * * *", &last, time.Now()) {
		t.Error("expected shouldRun=false when last run was 30s ago and schedule is hourly")
	}
}

func TestShouldRun_DueToFire(t *testing.T) {
	// "every minute" schedule, last run 2 minutes ago → should fire.
	last := time.Now().Add(-2 * time.Minute).Format(time.RFC3339)
	if !shouldRun("* * * * *", &last, time.Now()) {
		t.Error("expected shouldRun=true when last run was 2 minutes ago")
	}
}

func TestShouldRun_InvalidCron(t *testing.T) {
	if shouldRun("invalid-cron", nil, time.Now()) {
		t.Error("expected shouldRun=false for invalid cron expression")
	}
}

func TestShouldRun_EmptyLastRun(t *testing.T) {
	empty := ""
	if !shouldRun("* * * * *", &empty, time.Now()) {
		t.Error("expected shouldRun=true when lastRun is empty string")
	}
}

func TestShouldRun_DailyNotYet(t *testing.T) {
	// Daily at midnight, last run was today at 00:01, now is 10:00.
	now := time.Date(2026, 3, 26, 10, 0, 0, 0, time.UTC)
	last := time.Date(2026, 3, 26, 0, 1, 0, 0, time.UTC).Format(time.RFC3339)
	if shouldRun("0 0 * * *", &last, now) {
		t.Error("expected shouldRun=false when daily schedule already ran today")
	}
}

func TestShouldRun_DailyDue(t *testing.T) {
	// Daily at midnight, last run was yesterday.
	now := time.Date(2026, 3, 27, 0, 1, 0, 0, time.UTC)
	last := time.Date(2026, 3, 26, 0, 1, 0, 0, time.UTC).Format(time.RFC3339)
	if !shouldRun("0 0 * * *", &last, now) {
		t.Error("expected shouldRun=true when daily schedule is past due")
	}
}

func TestTriggerRefresh_NoActiveBundle(t *testing.T) {
	srv := setupRefreshTest(t)
	app := &db.AppRow{ID: "app-1", ActiveBundle: nil}

	// Should return immediately without error.
	srv.triggerRefresh(context.Background(), app)
}

func TestTriggerRefresh_PinnedManifest(t *testing.T) {
	srv := setupRefreshTest(t)
	bundleID := "bundle-1"
	app := &db.AppRow{
		ID:           "app-1",
		ActiveBundle: &bundleID,
	}

	// Set up bundle directory with a pinned manifest (has packages).
	bundlePaths := srv.BundlePaths("app-1", bundleID)
	os.MkdirAll(bundlePaths.Base, 0o755)

	m := &manifest.Manifest{
		Version: 1,
		Packages: map[string]manifest.Package{
			"shiny": {Package: "shiny", Version: "1.0"},
		},
	}
	data, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(bundlePaths.Base, "manifest.json"), data, 0o644)

	// Pinned manifest → should skip refresh.
	srv.triggerRefresh(context.Background(), app)
}

func TestTriggerRefresh_ManifestReadError(t *testing.T) {
	srv := setupRefreshTest(t)
	bundleID := "bundle-1"
	app := &db.AppRow{
		ID:           "app-1",
		ActiveBundle: &bundleID,
	}

	// No manifest file exists → should log warning and return.
	srv.triggerRefresh(context.Background(), app)
}

func TestRunRefreshScheduler_StopsOnCancel(t *testing.T) {
	srv := setupRefreshTest(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	done := make(chan struct{})
	go func() {
		srv.RunRefreshScheduler(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Returned as expected.
	case <-time.After(2 * time.Second):
		t.Fatal("RunRefreshScheduler did not return after context cancel")
	}
}

func TestRunRefresh_UnchangedDeps(t *testing.T) {
	srv := setupRefreshTest(t)
	bundleID := "bundle-1"
	app := &db.AppRow{
		ID:           "app-1",
		ActiveBundle: &bundleID,
	}

	bundlePaths := srv.BundlePaths("app-1", bundleID)
	os.MkdirAll(bundlePaths.Unpacked, 0o755)

	m := &manifest.Manifest{
		Metadata: manifest.Metadata{Entrypoint: "app.R"},
	}

	// The mock build succeeds but doesn't create any output files.
	// This means copyFile for the store-manifest will fail — task should be Failed.
	sender := srv.Tasks.Create("task-1", "app-1")
	changed := srv.RunRefresh(context.Background(), app, m, sender)

	if changed {
		t.Error("expected no change when build output is missing")
	}
	status, _ := srv.Tasks.Status("task-1")
	if status != task.Failed {
		t.Errorf("expected Failed, got %v", status)
	}
}
