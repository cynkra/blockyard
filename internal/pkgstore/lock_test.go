package pkgstore

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLockPath(t *testing.T) {
	s := NewStore("/data/.pkg-store")
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	got := s.LockPath("shiny", "abc123")
	want := "/data/.pkg-store/.locks/4.5-x86_64-pc-linux-gnu/shiny/abc123.lock"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAcquireRelease(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	ctx := context.Background()
	err := s.Acquire(ctx, "shiny", "src1", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Lock directory should exist.
	lockDir := s.LockPath("shiny", "src1")
	if _, err := os.Stat(lockDir); err != nil {
		t.Error("lock directory should exist after Acquire")
	}

	s.Release("shiny", "src1")

	// Lock directory should be gone.
	if _, err := os.Stat(lockDir); !os.IsNotExist(err) {
		t.Error("lock directory should not exist after Release")
	}
}

func TestAcquire_ContextCancelled(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// Hold a lock.
	ctx := context.Background()
	if err := s.Acquire(ctx, "shiny", "src1", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	defer s.Release("shiny", "src1")

	// Try to acquire the same lock with a cancelled context.
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.Acquire(cancelCtx, "shiny", "src1", 30*time.Minute)
	if err == nil {
		t.Error("expected error when context is cancelled")
	}
}

func TestAcquire_StaleLockRemoved(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// Create a stale lock directory.
	lockDir := s.LockPath("shiny", "src1")
	os.MkdirAll(lockDir, 0o755)
	staleTime := time.Now().Add(-2 * time.Hour)
	os.Chtimes(lockDir, staleTime, staleTime)

	ctx := context.Background()
	err := s.Acquire(ctx, "shiny", "src1", 1*time.Hour)
	if err != nil {
		t.Fatalf("should acquire after removing stale lock: %v", err)
	}
	defer s.Release("shiny", "src1")
}

func TestAcquire_WaitsForLock(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// Hold a lock.
	ctx := context.Background()
	if err := s.Acquire(ctx, "shiny", "src1", 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	// Release it after a short delay.
	go func() {
		time.Sleep(100 * time.Millisecond)
		s.Release("shiny", "src1")
	}()

	// Second acquire should succeed after the release.
	acquireCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.Acquire(acquireCtx, "shiny", "src1", 30*time.Minute)
	if err != nil {
		t.Fatalf("second acquire should succeed: %v", err)
	}
	s.Release("shiny", "src1")
}
