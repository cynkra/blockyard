package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cynkra/blockyard/internal/pkgstore"
)

func populateCmd() *cobra.Command {
	var lockfile, lib, storeRoot, refLib string
	var runtime bool
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

			if runtime && refManifest != nil {
				st, err := s.PopulateRuntime(lf, lib, refLib, refManifest)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr,
					"store: %d hits, %d misses, %d ABI hits, %d ABI rebuilds\n",
					st.Hits, st.Misses, st.ABIHits, st.ABIRebuilds)
				return nil
			}

			st, err := s.PopulateBuild(lf, lib, refManifest)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "store: %d hits, %d misses\n", st.Hits, st.Misses)
			return nil
		},
	}
	cmd.Flags().StringVar(&lockfile, "lockfile", "", "path to pak.lock")
	cmd.Flags().StringVar(&lib, "lib", "", "build library path")
	cmd.Flags().StringVar(&storeRoot, "store", "", "store root path")
	cmd.Flags().StringVar(&refLib, "reference-lib", "", "skip packages present here (optional)")
	cmd.Flags().BoolVar(&runtime, "runtime", false, "runtime mode: pre-populate from worker library with ABI check")
	_ = cmd.MarkFlagRequired("lockfile")
	_ = cmd.MarkFlagRequired("lib")
	_ = cmd.MarkFlagRequired("store")
	return cmd
}
