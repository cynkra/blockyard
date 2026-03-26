package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/cynkra/blockyard/internal/pkgstore"
)

func ingestCmd() *cobra.Command {
	var lockfile, lib, storeRoot, refLib string
	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Ingest newly installed packages into the store",
		RunE: func(cmd *cobra.Command, args []string) error {
			lf, err := pkgstore.ReadLockfile(lockfile)
			if err != nil {
				return err
			}
			s := pkgstore.NewStore(storeRoot)
			s.SetPlatform(pkgstore.PlatformFromLockfile(lf))

			// Load the reference library's package manifest to compare
			// compound store refs.
			var refManifest map[string]string
			if refLib != "" {
				refManifest, _ = pkgstore.ReadPackageManifest(refLib)
			}

			// Build the store-manifest as we go.
			storeManifest := make(map[string]string)

			for _, entry := range lf.Packages {
				if entry.IsMetaEntry() {
					continue
				}
				sourceHash, err := pkgstore.StoreKey(entry)
				if err != nil {
					return err
				}

				pkgPath := filepath.Join(lib, entry.Package)
				if !dirExists(pkgPath) {
					// Package not in --lib — carry ref from reference lib.
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
					pkgstore.IngestContext(descPath, lf)
				if err != nil {
					return fmt.Errorf("ingest context for %s: %w",
						entry.Package, err)
				}

				// Record in store-manifest regardless of ingestion.
				storeManifest[entry.Package] = pkgstore.StoreRef(sourceHash, configHash)

				// Skip packages whose compound ref matches the reference
				// library.
				expectedRef := pkgstore.StoreRef(sourceHash, configHash)
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
					if err := s.Acquire(cmd.Context(), entry.Package, sourceHash, 30*time.Minute); err != nil {
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
					return err
				}

				fmt.Fprintf(os.Stderr, "store: ingested %s (config %s)\n",
					entry.Package, configHash[:12])
			}

			// Write store-manifest.
			return pkgstore.WriteStoreManifest(lib, storeManifest)
		},
	}
	cmd.Flags().StringVar(&lockfile, "lockfile", "", "path to pak.lock")
	cmd.Flags().StringVar(&lib, "lib", "", "build library path")
	cmd.Flags().StringVar(&storeRoot, "store", "", "store root path")
	cmd.Flags().StringVar(&refLib, "reference-lib", "", "skip packages present here (optional)")
	_ = cmd.MarkFlagRequired("lockfile")
	_ = cmd.MarkFlagRequired("lib")
	_ = cmd.MarkFlagRequired("store")
	return cmd
}
