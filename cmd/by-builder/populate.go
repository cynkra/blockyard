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
				return populateRuntime(s, lf, lib, refLib, refManifest)
			}

			return populateBuild(s, lf, lib, refManifest)
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

// populateBuild is the standard build-time populate: for each lockfile
// entry, check the store for a matching config and hardlink hits into
// the build library.
func populateBuild(
	s *pkgstore.Store, lf *pkgstore.Lockfile,
	lib string, refManifest map[string]string,
) error {
	var hits, misses int
	for _, entry := range lf.Packages {
		if entry.IsMetaEntry() {
			continue
		}
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
}

// populateRuntime extends the build-time populate with worker library
// pre-population and the reverse-LinkingTo ABI check. After this
// function, the staging directory contains a complete library minus
// packages that need ABI recompilation.
func populateRuntime(
	s *pkgstore.Store, lf *pkgstore.Lockfile,
	lib, refLib string, refManifest map[string]string,
) error {
	// 1. Compute store keys for all lockfile entries.
	newKeys := make(map[string]string) // package → source store key
	for _, entry := range lf.Packages {
		if entry.IsMetaEntry() {
			continue
		}
		key, err := pkgstore.StoreKey(entry)
		if err != nil {
			return err
		}
		newKeys[entry.Package] = key
	}

	// 2. Identify changed packages: store key differs from the
	//    worker's .packages.json.
	changed := make(map[string]bool)
	for pkg, newKey := range newKeys {
		oldRef, ok := refManifest[pkg]
		if !ok {
			continue // new package, not in worker lib
		}
		oldSource, _, _ := pkgstore.SplitStoreRef(oldRef)
		if oldSource != newKey {
			changed[pkg] = true
		}
	}

	var hits, misses, abiHits, abiRebuilds int

	// 3. For each unchanged package in the worker library, check
	//    whether its LinkingTo deps include any changed package.
	for pkg, ref := range refManifest {
		if changed[pkg] {
			continue // handled below with new/changed packages
		}

		// Skip if already in staging (shouldn't happen but be safe).
		if dirExists(filepath.Join(lib, pkg)) {
			continue
		}

		sourceHash, _, _ := pkgstore.SplitStoreRef(ref)

		sc, err := pkgstore.ReadStoreConfigs(s.ConfigsPath(pkg, sourceHash))
		if err != nil || len(sc.LinkingTo) == 0 {
			// No configs.json or no LinkingTo deps — safe.
			// Hardlink from worker lib into staging.
			if err := hardlinkDir(refLib, pkg, lib); err != nil {
				return fmt.Errorf("link unchanged %s from worker lib: %w", pkg, err)
			}
			continue
		}

		// Check if any LinkingTo dep changed.
		affected := false
		for _, linkedPkg := range sc.LinkingTo {
			if changed[linkedPkg] {
				affected = true
				break
			}
		}

		if !affected {
			// LinkingTo deps unchanged — hardlink from worker lib.
			if err := hardlinkDir(refLib, pkg, lib); err != nil {
				return fmt.Errorf("link unchanged %s from worker lib: %w", pkg, err)
			}
			continue
		}

		// Affected: try to find a new config in the store compiled
		// against the new LinkingTo store keys.
		newConfigHash, ok := s.ResolveConfig(pkg, sourceHash, lf)
		if ok {
			// Store hit — hardlink the new config into staging.
			dest := filepath.Join(lib, pkg)
			out, cpErr := exec.Command(
				"cp", "-al",
				s.Path(pkg, sourceHash, newConfigHash), dest,
			).CombinedOutput()
			if cpErr != nil {
				return fmt.Errorf("link ABI-updated %s: %s: %w", pkg, out, cpErr)
			}
			s.Touch(pkg, sourceHash, newConfigHash)
			abiHits++
		} else {
			// Store miss — exclude from staging. pak will reinstall
			// this package in phase 3, compiling against the new
			// headers already present in staging.
			abiRebuilds++
			fmt.Fprintf(os.Stderr,
				"store: ABI rebuild needed for %s (LinkingTo dep changed)\n",
				pkg)
		}
	}

	// 4. Handle new/changed packages as in the build-time path:
	//    check store for matching configs, hardlink hits into staging.
	for _, entry := range lf.Packages {
		if entry.IsMetaEntry() {
			continue
		}
		sourceHash := newKeys[entry.Package]

		// Only process packages that are new or changed.
		if _, isRef := refManifest[entry.Package]; isRef && !changed[entry.Package] {
			continue // already handled above
		}

		// Skip if already in staging.
		if dirExists(filepath.Join(lib, entry.Package)) {
			continue
		}

		configHash, ok := s.ResolveConfig(entry.Package, sourceHash, lf)
		if !ok {
			misses++
			continue
		}

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

	fmt.Fprintf(os.Stderr,
		"store: %d hits, %d misses, %d ABI hits, %d ABI rebuilds\n",
		hits, misses, abiHits, abiRebuilds)
	return nil
}

// hardlinkDir hardlinks a package directory from srcLib into destLib.
func hardlinkDir(srcLib, pkg, destLib string) error {
	src := filepath.Join(srcLib, pkg)
	dest := filepath.Join(destLib, pkg)
	out, err := exec.Command("cp", "-al", src, dest).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
