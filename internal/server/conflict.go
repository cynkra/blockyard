package server

import (
	"github.com/cynkra/blockyard/internal/pkgstore"
)

// detectConflict checks whether the new store-manifest conflicts with
// the R session's loaded namespaces by comparing compound store refs
// from the worker's .packages.json manifest against those in the
// store-manifest (written by `by-builder store ingest`). The compound
// ref encodes both the source identity (version/sha256) and the ABI
// configuration (LinkingTo store keys), catching both version changes
// and LinkingTo recompilation needs.
func detectConflict(
	storeManifestPath string,
	workerManifest map[string]string,
	loadedNamespaces []string,
) (conflict bool, pkg string, err error) {
	newRefs, err := pkgstore.ReadStoreManifest(storeManifestPath)
	if err != nil {
		return false, "", err
	}

	for _, ns := range loadedNamespaces {
		currentRef, installed := workerManifest[ns]
		if !installed {
			continue
		}
		newRef, inNewManifest := newRefs[ns]
		if !inNewManifest {
			continue
		}
		if currentRef != newRef {
			return true, ns, nil
		}
	}
	return false, "", nil
}
