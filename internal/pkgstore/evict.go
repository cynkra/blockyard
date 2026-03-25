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
func (s *Store) EvictStale(retention time.Duration) (int, error) {
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
			n, err := s.evictSourceHash(pkgEntry.Name(), shEntry.Name(), shDir, cutoff)
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
// hash directory.
func (s *Store) evictSourceHash(pkg, sourceHash, shDir string, cutoff time.Time) (int, error) {
	removed := 0
	entries, err := os.ReadDir(shDir)
	if err != nil {
		return 0, err
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || entry.Name() == "configs.json" {
			continue
		}
		configHash := strings.TrimSuffix(entry.Name(), ".json")

		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // still fresh
		}

		// Remove config directory and sidecar.
		configDir := filepath.Join(shDir, configHash)
		_ = os.RemoveAll(configDir)
		_ = os.Remove(filepath.Join(shDir, entry.Name()))

		// Update configs.json: remove the config entry.
		configsPath := s.ConfigsPath(pkg, sourceHash)
		if sc, err := ReadStoreConfigs(configsPath); err == nil {
			delete(sc.Configs, configHash)
			if len(sc.Configs) == 0 {
				_ = os.Remove(configsPath)
			} else {
				_ = WriteStoreConfigs(configsPath, sc)
			}
		}

		removed++
		slog.Info("evicted stale config",
			"package", pkg,
			"source_hash", sourceHash,
			"config_hash", configHash,
			"last_accessed", info.ModTime())
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
				n, err := store.EvictStale(retention)
				if err != nil {
					slog.Warn("store eviction sweep error", "error", err)
				} else if n > 0 {
					slog.Info("store eviction sweep complete", "removed", n)
				}
			}
		}
	}()
}
