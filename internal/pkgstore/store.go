package pkgstore

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Store is a content-addressable package store keyed by
// {platform}/{package}/{source_hash}/{config_hash}.
type Store struct {
	root     string // host-side store root, e.g., {bundle_server_path}/.pkg-store
	platform string // e.g., 4.5-x86_64-pc-linux-gnu; set via PlatformFromLockfile
}

func NewStore(root string) *Store {
	return &Store{root: root}
}

func (s *Store) Root() string     { return s.root }
func (s *Store) Platform() string { return s.platform }

func (s *Store) SetPlatform(p string) { s.platform = p }

// SourceDir returns the source-hash directory for a package.
func (s *Store) SourceDir(pkg, sourceHash string) string {
	return filepath.Join(s.root, s.platform, pkg, sourceHash)
}

// Path returns the config directory (installed package tree) path.
func (s *Store) Path(pkg, sourceHash, configHash string) string {
	return filepath.Join(s.root, s.platform, pkg, sourceHash, configHash)
}

// ConfigsPath returns the path to configs.json for a source hash.
func (s *Store) ConfigsPath(pkg, sourceHash string) string {
	return filepath.Join(s.root, s.platform, pkg, sourceHash, "configs.json")
}

// ConfigMetaPath returns the config sidecar file path.
func (s *Store) ConfigMetaPath(pkg, sourceHash, configHash string) string {
	return filepath.Join(s.root, s.platform, pkg, sourceHash, configHash+".json")
}

// Has reports whether the store contains a specific config for a package.
func (s *Store) Has(pkg, sourceHash, configHash string) bool {
	_, err := os.Stat(s.Path(pkg, sourceHash, configHash))
	return err == nil
}

// Ingest atomically moves an installed package tree into the store
// as a config entry. No-op if the config already exists. srcDir must
// be on the same filesystem as the store (for atomic rename).
func (s *Store) Ingest(pkg, sourceHash, configHash, srcDir string) error {
	dst := s.Path(pkg, sourceHash, configHash)
	if dirExists(dst) {
		return nil // already in store
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // G301: package store dir, not secrets
		return fmt.Errorf("create store dir: %w", err)
	}
	return os.Rename(srcDir, dst)
}

// Touch updates the mtime of a config's sidecar file.
// Used for last-accessed tracking — the eviction sweeper removes
// config entries whose sidecar mtime exceeds the retention window.
func (s *Store) Touch(pkg, sourceHash, configHash string) {
	metaPath := s.ConfigMetaPath(pkg, sourceHash, configHash)
	now := time.Now()
	_ = os.Chtimes(metaPath, now, now)
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
