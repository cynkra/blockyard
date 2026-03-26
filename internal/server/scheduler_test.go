package server

import (
	"testing"
	"time"
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
