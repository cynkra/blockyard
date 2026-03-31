package pkgstore

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

// LockPath returns the lock directory path for a package source hash.
func (s *Store) LockPath(pkg, sourceHash string) string {
	return filepath.Join(s.root, ".locks", s.platform, pkg, sourceHash+".lock")
}

// Acquire takes a file-based lock for a package source hash.
// Blocks with jittered backoff until the lock is acquired or the
// context is cancelled. Stale locks older than staleThreshold are
// removed and re-attempted.
func (s *Store) Acquire(ctx context.Context, pkg, sourceHash string, staleThreshold time.Duration) error {
	lockDir := s.LockPath(pkg, sourceHash)
	_ = os.MkdirAll(filepath.Dir(lockDir), 0o755) //nolint:gosec // G301: lock dir, not secrets

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("lock acquisition cancelled for %s: %w", pkg, err)
		}
		if err := os.Mkdir(lockDir, 0o755); err == nil { //nolint:gosec // G301: lock dir, not secrets
			return nil // acquired
		}
		// Check for stale lock.
		info, err := os.Stat(lockDir)
		if err == nil && time.Since(info.ModTime()) > staleThreshold {
			_ = os.RemoveAll(lockDir)
			continue
		}
		// Wait with jittered backoff.
		jitter := time.Duration(500+rand.IntN(1500)) * time.Millisecond //nolint:gosec // G404: retry jitter, not security-sensitive
		select {
		case <-ctx.Done():
			return fmt.Errorf("lock acquisition cancelled for %s: %w", pkg, ctx.Err())
		case <-time.After(jitter):
		}
	}
}

// Release removes the lock directory for a package source hash.
func (s *Store) Release(pkg, sourceHash string) {
	_ = os.RemoveAll(s.LockPath(pkg, sourceHash))
}
