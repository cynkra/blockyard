package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cynkra/blockyard/internal/pkgstore"
)

func populateCmd() *cobra.Command {
	var lockfile, lib, storeRoot, refLib string
	cmd := &cobra.Command{
		Use:   "populate",
		Short: "Pre-populate build library from the package store",
		RunE: func(cmd *cobra.Command, args []string) error {
			lf, err := pkgstore.ReadLockfile(lockfile)
			if err != nil {
				return err
			}
			s := pkgstore.NewStore(storeRoot)
			s.SetPlatform(pkgstore.PlatformFromLockfile(lf))

			// Load the reference library's package manifest to compare
			// compound store refs (sourceHash/configHash).
			var refManifest map[string]string
			if refLib != "" {
				refManifest, _ = pkgstore.ReadPackageManifest(refLib)
			}

			var hits, misses int
			for _, entry := range lf.Packages {
				sourceHash, err := pkgstore.StoreKey(entry)
				if err != nil {
					return err
				}

				// Skip packages already in the build/staging library.
				if dirExists(filepath.Join(lib, entry.Package)) {
					continue
				}

				// Resolve matching config via configs.json.
				configHash, ok := s.ResolveConfig(entry.Package, sourceHash, lf)
				if !ok {
					misses++
					continue
				}

				// Skip packages whose compound ref matches the reference
				// library.
				expectedRef := pkgstore.StoreRef(sourceHash, configHash)
				if refManifest != nil && refManifest[entry.Package] == expectedRef {
					continue
				}

				// Hard-link config's package tree into build library.
				dest := filepath.Join(lib, entry.Package)
				out, cpErr := exec.Command(
					"cp", "-al",
					s.Path(entry.Package, sourceHash, configHash), dest,
				).CombinedOutput()
				if cpErr != nil {
					return fmt.Errorf("link %s: %s: %w", entry.Package, out, cpErr)
				}
				s.Touch(entry.Package, sourceHash, configHash)
				hits++
			}

			fmt.Fprintf(os.Stderr, "store: %d hits, %d misses\n", hits, misses)
			return nil
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

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
