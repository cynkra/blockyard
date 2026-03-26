package pkgstore

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// CreateStagingDir creates a staging directory under the store root.
// Used by runtime package installation to provide a working directory
// on the same filesystem as the store (enabling atomic rename and hardlinks).
func (s *Store) CreateStagingDir() (string, error) {
	dir := filepath.Join(s.root, ".staging", uuid.New().String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	return dir, nil
}

// CleanupStagingDir removes a staging directory.
func (s *Store) CleanupStagingDir(dir string) error {
	return os.RemoveAll(dir)
}
