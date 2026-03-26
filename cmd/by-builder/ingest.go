package main

import (
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

			storeManifest, err := s.IngestPackages(cmd.Context(), lf, lib, refManifest)
			if err != nil {
				return err
			}

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
