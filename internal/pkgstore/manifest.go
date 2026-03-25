package pkgstore

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// packageManifestFile is the filename for the per-worker package manifest.
const packageManifestFile = ".packages.json"

// WritePackageManifest writes a per-worker package manifest to libDir.
func WritePackageManifest(libDir string, manifest map[string]string) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(libDir, packageManifestFile), data, 0o644)
}

// ReadPackageManifest reads the per-worker package manifest.
func ReadPackageManifest(libDir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(libDir, packageManifestFile))
	if err != nil {
		return nil, err
	}
	var manifest map[string]string
	return manifest, json.Unmarshal(data, &manifest)
}

// UpdatePackageManifest adds entries to the per-worker package manifest.
// Existing entries are preserved; additions overwrite on key collision.
func UpdatePackageManifest(libDir string, additions map[string]string) error {
	manifest, err := ReadPackageManifest(libDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if manifest == nil {
		manifest = make(map[string]string)
	}
	for pkg, key := range additions {
		manifest[pkg] = key
	}
	return WritePackageManifest(libDir, manifest)
}

// storeManifestFile is the filename for the per-bundle store-manifest.
const storeManifestFile = "store-manifest.json"

// WriteStoreManifest writes the store-manifest ({package: "sourceHash/configHash"})
// to the given directory. Called by `store ingest` at the end of a build.
func WriteStoreManifest(dir string, manifest map[string]string) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, storeManifestFile), data, 0o644)
}

// ReadStoreManifest reads a store-manifest from the given path.
// The path should be the full file path (e.g., "{bundle}/store-manifest.json").
func ReadStoreManifest(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest map[string]string
	return manifest, json.Unmarshal(data, &manifest)
}
