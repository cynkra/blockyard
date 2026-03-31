package pkgstore

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

// PopulateStats reports cache hit/miss counts from a populate run.
type PopulateStats struct {
	Hits         int
	Misses       int
	ABIHits      int
	ABIRebuilds  int
}

// PopulateBuild is the standard build-time populate: for each lockfile
// entry, check the store for a matching config and hardlink hits into
// the build library.
func (s *Store) PopulateBuild(
	lf *Lockfile, lib string, refManifest map[string]string,
) (PopulateStats, error) {
	var st PopulateStats
	for _, entry := range lf.Packages {
		if entry.IsMetaEntry() {
			continue
		}
		sourceHash, err := StoreKey(entry)
		if err != nil {
			return st, err
		}

		// Skip packages already in the build/staging library.
		if dirExists(filepath.Join(lib, entry.Package)) {
			continue
		}

		// Resolve matching config via configs.json.
		configHash, ok := s.ResolveConfig(entry.Package, sourceHash, lf)
		if !ok {
			st.Misses++
			continue
		}

		// Skip packages whose compound ref matches the reference
		// library.
		expectedRef := StoreRef(sourceHash, configHash)
		if refManifest != nil && refManifest[entry.Package] == expectedRef {
			continue
		}

		// Hard-link config's package tree into build library.
		dest := filepath.Join(lib, entry.Package)
		out, cpErr := exec.Command( //nolint:gosec // G204: controlled cp hardlink for package store
			"cp", "-al",
			s.Path(entry.Package, sourceHash, configHash), dest,
		).CombinedOutput()
		if cpErr != nil {
			return st, fmt.Errorf("link %s: %s: %w", entry.Package, out, cpErr)
		}
		s.Touch(entry.Package, sourceHash, configHash)
		st.Hits++
	}
	return st, nil
}

// PopulateRuntime extends the build-time populate with worker library
// pre-population and the reverse-LinkingTo ABI check. After this
// function, the staging directory contains a complete library minus
// packages that need ABI recompilation.
func (s *Store) PopulateRuntime(
	lf *Lockfile, lib, refLib string, refManifest map[string]string,
) (PopulateStats, error) {
	var st PopulateStats

	// 1. Compute store keys for all lockfile entries.
	newKeys := make(map[string]string) // package -> source store key
	for _, entry := range lf.Packages {
		if entry.IsMetaEntry() {
			continue
		}
		key, err := StoreKey(entry)
		if err != nil {
			return st, err
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
		oldSource, _, _ := SplitStoreRef(oldRef)
		if oldSource != newKey {
			changed[pkg] = true
		}
	}

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

		sourceHash, _, _ := SplitStoreRef(ref)

		sc, err := ReadStoreConfigs(s.ConfigsPath(pkg, sourceHash))
		if err != nil || len(sc.LinkingTo) == 0 {
			// No configs.json or no LinkingTo deps -- safe.
			// Hardlink from worker lib into staging.
			if err := hardlinkDir(refLib, pkg, lib); err != nil {
				return st, fmt.Errorf("link unchanged %s from worker lib: %w", pkg, err)
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
			// LinkingTo deps unchanged -- hardlink from worker lib.
			if err := hardlinkDir(refLib, pkg, lib); err != nil {
				return st, fmt.Errorf("link unchanged %s from worker lib: %w", pkg, err)
			}
			continue
		}

		// Affected: try to find a new config in the store compiled
		// against the new LinkingTo store keys.
		newConfigHash, ok := s.ResolveConfig(pkg, sourceHash, lf)
		if ok {
			// Store hit -- hardlink the new config into staging.
			dest := filepath.Join(lib, pkg)
			out, cpErr := exec.Command( //nolint:gosec // G204: controlled cp hardlink for package store
				"cp", "-al",
				s.Path(pkg, sourceHash, newConfigHash), dest,
			).CombinedOutput()
			if cpErr != nil {
				return st, fmt.Errorf("link ABI-updated %s: %s: %w", pkg, out, cpErr)
			}
			s.Touch(pkg, sourceHash, newConfigHash)
			st.ABIHits++
		} else {
			// Store miss -- exclude from staging. pak will reinstall
			// this package in phase 3, compiling against the new
			// headers already present in staging.
			st.ABIRebuilds++
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
			st.Misses++
			continue
		}

		dest := filepath.Join(lib, entry.Package)
		out, cpErr := exec.Command( //nolint:gosec // G204: controlled cp hardlink for package store
			"cp", "-al",
			s.Path(entry.Package, sourceHash, configHash), dest,
		).CombinedOutput()
		if cpErr != nil {
			return st, fmt.Errorf("link %s: %s: %w", entry.Package, out, cpErr)
		}
		s.Touch(entry.Package, sourceHash, configHash)
		st.Hits++
	}

	return st, nil
}

// hardlinkDir hardlinks a package directory from srcLib into destLib.
func hardlinkDir(srcLib, pkg, destLib string) error {
	src := filepath.Join(srcLib, pkg)
	dest := filepath.Join(destLib, pkg)
	out, err := exec.Command("cp", "-al", src, dest).CombinedOutput() //nolint:gosec // G204: controlled cp hardlink for package store
	if err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}
