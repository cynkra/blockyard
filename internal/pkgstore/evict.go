package pkgstore

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EvictStale removes config entries whose sidecar mtime is older than
// the retention cutoff. Returns the number of configs removed.
func (s *Store) EvictStale(ctx context.Context, retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-retention)
	removed := 0

	platformDir := filepath.Join(s.root, s.platform)
	packages, err := os.ReadDir(platformDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	for _, pkgEntry := range packages {
		if !pkgEntry.IsDir() || strings.HasPrefix(pkgEntry.Name(), ".") {
			continue
		}
		pkgDir := filepath.Join(platformDir, pkgEntry.Name())
		sourceHashes, _ := os.ReadDir(pkgDir)

		for _, shEntry := range sourceHashes {
			if !shEntry.IsDir() {
				continue
			}
			shDir := filepath.Join(pkgDir, shEntry.Name())
			n, err := s.evictSourceHash(ctx, pkgEntry.Name(), shEntry.Name(), shDir, cutoff)
			if err != nil {
				slog.Warn("eviction error",
					"package", pkgEntry.Name(),
					"source_hash", shEntry.Name(),
					"error", err)
				continue
			}
			removed += n
		}

		removeIfEmpty(pkgDir)
	}
	return removed, nil
}

// evictSourceHash removes expired config entries under a single source
// hash directory. Acquires the per-source-hash lock to prevent races
// with concurrent ingestion writing to configs.json.
func (s *Store) evictSourceHash(ctx context.Context, pkg, sourceHash, shDir string, cutoff time.Time) (int, error) {
	// Phase 1: scan for stale candidates without the lock (read-only).
	entries, err := os.ReadDir(shDir)
	if err != nil {
		return 0, err
	}

	type candidate struct {
		configHash string
	}
	var candidates []candidate

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || entry.Name() == "configs.json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // still fresh
		}
		candidates = append(candidates, candidate{
			configHash: strings.TrimSuffix(entry.Name(), ".json"),
		})
	}

	if len(candidates) == 0 {
		return 0, nil
	}

	// Phase 2: acquire lock, re-check mtimes, and evict.
	if err := s.Acquire(ctx, pkg, sourceHash, 30*time.Minute); err != nil {
		return 0, err
	}
	defer s.Release(pkg, sourceHash)

	removed := 0
	var evictedHashes []string

	for _, c := range candidates {
		// Re-check mtime under lock — a concurrent Touch may have
		// refreshed the sidecar since the unlocked scan.
		sidecarPath := s.ConfigMetaPath(pkg, sourceHash, c.configHash)
		info, err := os.Stat(sidecarPath)
		if err != nil {
			continue // sidecar gone (concurrent removal)
		}
		if info.ModTime().After(cutoff) {
			continue // refreshed since scan
		}

		// Remove config directory and sidecar.
		_ = os.RemoveAll(s.Path(pkg, sourceHash, c.configHash))
		_ = os.Remove(sidecarPath)
		evictedHashes = append(evictedHashes, c.configHash)
		removed++

		slog.Info("evicted stale config",
			"package", pkg,
			"source_hash", sourceHash,
			"config_hash", c.configHash,
			"last_accessed", info.ModTime())
	}

	// Batch-update configs.json under the lock.
	if len(evictedHashes) > 0 {
		configsPath := s.ConfigsPath(pkg, sourceHash)
		if sc, err := ReadStoreConfigs(configsPath); err == nil {
			for _, ch := range evictedHashes {
				delete(sc.Configs, ch)
			}
			if len(sc.Configs) == 0 {
				_ = os.Remove(configsPath)
			} else {
				_ = WriteStoreConfigs(configsPath, sc)
			}
		}
	}

	removeIfEmpty(shDir)
	return removed, nil
}

func removeIfEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
}

// SpawnEvictionSweeper starts a background goroutine that periodically
// evicts stale store entries.
func SpawnEvictionSweeper(ctx context.Context, store *Store, retention time.Duration) {
	if retention <= 0 {
		return
	}

	interval := 24 * time.Hour
	if retention < 24*time.Hour {
		interval = retention / 2
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := store.EvictStale(ctx, retention)
				if err != nil {
					slog.Warn("store eviction sweep error", "error", err)
				} else if n > 0 {
					slog.Info("store eviction sweep complete", "removed", n)
				}
			}
		}
	}()
}
