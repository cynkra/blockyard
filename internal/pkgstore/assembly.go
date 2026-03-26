package pkgstore

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SplitStoreRef splits a compound store ref "sourceHash/configHash"
// into its two components. Returns an error if the ref is malformed.
func SplitStoreRef(ref string) (sourceHash, configHash string, err error) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("malformed store ref: %q", ref)
	}
	return parts[0], parts[1], nil
}

// AssembleLibrary creates a library directory by hard-linking packages
// from the store based on a pre-computed store-manifest. After linking,
// it writes a .packages.json manifest initialized as a copy of the
// store-manifest.
func (s *Store) AssembleLibrary(
	libDir string, storeManifest map[string]string,
) (missing []string, err error) {
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		return nil, fmt.Errorf("create lib dir: %w", err)
	}

	for pkg, ref := range storeManifest {
		sourceHash, configHash, err := SplitStoreRef(ref)
		if err != nil {
			return nil, fmt.Errorf("store ref for %s: %w", pkg, err)
		}

		storePath := s.Path(pkg, sourceHash, configHash)
		if !dirExists(storePath) {
			missing = append(missing, pkg)
			continue
		}

		destPath := filepath.Join(libDir, pkg)
		out, cpErr := exec.Command(
			"cp", "-al", storePath, destPath,
		).CombinedOutput()
		if cpErr != nil {
			return nil, fmt.Errorf(
				"hard-link %s: %s: %w", pkg, out, cpErr)
		}

		s.Touch(pkg, sourceHash, configHash)
	}

	if err := WritePackageManifest(libDir, storeManifest); err != nil {
		return nil, fmt.Errorf("write package manifest: %w", err)
	}

	return missing, nil
}

// WorkerLibDir returns the host-side library directory for a worker.
func (s *Store) WorkerLibDir(workerID string) string {
	return filepath.Join(s.root, ".workers", workerID)
}

// CleanupWorkerLib removes a worker's library directory.
func (s *Store) CleanupWorkerLib(workerID string) error {
	dir := s.WorkerLibDir(workerID)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	return os.RemoveAll(dir)
}
