package pkgstore

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"
)

// IngestPackages iterates over lockfile entries, ingests any new or
// changed packages into the store, and returns the complete
// store-manifest map. Packages already present in refManifest with
// matching compound refs are skipped. Remaining refManifest entries
// are carried forward so the manifest is complete for container
// transfer.
func (s *Store) IngestPackages(ctx context.Context, lf *Lockfile, lib string, refManifest map[string]string) (map[string]string, error) {
	storeManifest := make(map[string]string)

	for _, entry := range lf.Packages {
		if entry.IsMetaEntry() {
			continue
		}
		sourceHash, err := StoreKey(entry)
		if err != nil {
			return nil, err
		}

		pkgPath := filepath.Join(lib, entry.Package)
		if !dirExists(pkgPath) {
			// Package not in lib — carry ref from reference lib.
			if refManifest != nil {
				if ref, ok := refManifest[entry.Package]; ok {
					storeManifest[entry.Package] = ref
				}
			}
			continue
		}

		// Compute config hash from installed DESCRIPTION.
		descPath := filepath.Join(pkgPath, "DESCRIPTION")
		configHash, linkingToKeys, sourceCompiled, linkingToNames, err :=
			IngestContext(descPath, lf)
		if err != nil {
			return nil, fmt.Errorf("ingest context for %s: %w",
				entry.Package, err)
		}

		// Record in store-manifest regardless of ingestion.
		storeManifest[entry.Package] = StoreRef(sourceHash, configHash)

		// Skip packages whose compound ref matches the reference
		// library.
		expectedRef := StoreRef(sourceHash, configHash)
		if refManifest != nil && refManifest[entry.Package] == expectedRef {
			continue
		}

		// Check whether this config is fully present in the store.
		if s.Has(entry.Package, sourceHash, configHash) {
			if _, ok := s.ResolveConfig(entry.Package, sourceHash, lf); ok {
				continue // fully consistent — skip
			}
			// Directory exists but metadata incomplete — fall
			// through to repair under lock.
		}

		// Ingest (or repair) under lock.
		if err := func() error {
			if err := s.Acquire(ctx, entry.Package, sourceHash, 30*time.Minute); err != nil {
				return fmt.Errorf("acquire lock for %s: %w", entry.Package, err)
			}
			defer s.Release(entry.Package, sourceHash)

			if !s.Has(entry.Package, sourceHash, configHash) {
				if err := s.Ingest(entry.Package, sourceHash, configHash, pkgPath); err != nil {
					return fmt.Errorf("ingest %s: %w", entry.Package, err)
				}
			}
			// Write/repair metadata — idempotent.
			if err := s.WriteIngestMeta(entry, lf,
				sourceHash, configHash, linkingToKeys,
				sourceCompiled, linkingToNames); err != nil {
				return fmt.Errorf("write ingest meta for %s: %w", entry.Package, err)
			}
			return nil
		}(); err != nil {
			return nil, err
		}

		slog.Info("store: ingested package",
			"package", entry.Package,
			"config", configHash[:12])
	}

	// Carry forward all remaining packages from the reference
	// library that aren't already in the store-manifest.
	// This makes the manifest complete for container transfer
	// (AssembleLibrary needs every package, not just the
	// lockfile's dependency subgraph).
	for pkg, ref := range refManifest {
		if _, exists := storeManifest[pkg]; !exists {
			storeManifest[pkg] = ref
		}
	}

	return storeManifest, nil
}
