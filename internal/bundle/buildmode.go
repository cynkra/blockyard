package bundle

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/cynkra/blockyard/internal/manifest"
)

// resolveManifest produces a manifest from whatever dependency metadata
// the bundle contains. Returns nil when only bare scripts are present
// (caller must run pre-processing first).
func resolveManifest(unpackedPath string) (*manifest.Manifest, error) {
	manifestPath := filepath.Join(unpackedPath, "manifest.json")
	if fileExists(manifestPath) {
		return manifest.Read(manifestPath)
	}

	meta := manifest.Metadata{
		AppMode:    detectAppMode(unpackedPath),
		Entrypoint: detectEntrypoint(unpackedPath),
	}
	files := computeFileChecksums(unpackedPath)

	renvLockPath := filepath.Join(unpackedPath, "renv.lock")
	if fileExists(renvLockPath) {
		return manifest.FromRenvLock(renvLockPath, meta, files)
	}

	descPath := filepath.Join(unpackedPath, "DESCRIPTION")
	if fileExists(descPath) {
		repos := defaultRepositories()
		return manifest.FromDescription(descPath, meta, files, repos)
	}

	return nil, nil // bare scripts — needs pre-processing
}

// detectAppMode infers the application mode from bundle contents.
func detectAppMode(unpackedPath string) string {
	if fileExists(filepath.Join(unpackedPath, "server.R")) &&
		fileExists(filepath.Join(unpackedPath, "ui.R")) {
		return "shiny"
	}
	if fileExists(filepath.Join(unpackedPath, "app.R")) {
		return "shiny"
	}
	return "shiny" // default
}

// detectEntrypoint returns the primary entrypoint file name.
func detectEntrypoint(unpackedPath string) string {
	if fileExists(filepath.Join(unpackedPath, "app.R")) {
		return "app.R"
	}
	if fileExists(filepath.Join(unpackedPath, "server.R")) {
		return "server.R"
	}
	return "app.R" // default
}

// computeFileChecksums walks the unpacked dir and computes SHA-256
// checksums for all regular files.
func computeFileChecksums(unpackedPath string) map[string]manifest.FileInfo {
	files := make(map[string]manifest.FileInfo)
	_ = filepath.WalkDir(unpackedPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(unpackedPath, path)
		if err != nil || strings.HasPrefix(rel, ".") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		files[rel] = manifest.FileInfo{
			Checksum: fmt.Sprintf("%x", sha256.Sum256(data)),
		}
		return nil
	})
	return files
}

// defaultRepositories returns the server's default repository configuration.
func defaultRepositories() []manifest.Repository {
	return []manifest.Repository{
		{Name: "CRAN", URL: "https://p3m.dev/cran/latest"},
	}
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
