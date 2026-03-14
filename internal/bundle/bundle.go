package bundle

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/cynkra/blockyard/internal/db"
)

// BuildContainerLibPath is the container-side mount point for the library
// volume in build containers. This must match the bind mount in the Docker
// backend. It is kept outside /app so that the bundle can be mounted
// read-only.
const BuildContainerLibPath = "/rv-library"

// Paths holds the filesystem locations for a bundle.
type Paths struct {
	Archive  string // {base}/{app_id}/{bundle_id}.tar.gz
	Unpacked string // {base}/{app_id}/{bundle_id}/
	Library  string // {base}/{app_id}/{bundle_id}_lib/
}

// NewBundlePaths constructs paths for a bundle. Single source of truth for
// the on-disk layout.
func NewBundlePaths(base, appID, bundleID string) Paths {
	appDir := filepath.Join(base, appID)
	return Paths{
		Archive:  filepath.Join(appDir, bundleID+".tar.gz"),
		Unpacked: filepath.Join(appDir, bundleID),
		Library:  filepath.Join(appDir, bundleID+"_lib"),
	}
}

// WriteArchive streams r to a temp file, then atomically renames it to
// the archive path. Creates the app directory if needed.
func WriteArchive(paths Paths, r io.Reader) error {
	appDir := filepath.Dir(paths.Archive)
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return fmt.Errorf("create app dir: %w", err)
	}

	// Temp file in the same directory for same-filesystem rename
	tmp, err := os.CreateTemp(appDir, ".bundle-*.tar.gz.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	// Clean up temp file on any error
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		return fmt.Errorf("write archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, paths.Archive); err != nil {
		return fmt.Errorf("rename archive: %w", err)
	}
	ok = true
	return nil
}

// UnpackArchive decompresses the tar.gz archive into the unpacked directory.
func UnpackArchive(paths Paths) error {
	f, err := os.Open(paths.Archive)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	if err := os.MkdirAll(paths.Unpacked, 0o755); err != nil {
		return fmt.Errorf("create unpack dir: %w", err)
	}

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		target := filepath.Join(paths.Unpacked, hdr.Name)

		// Prevent path traversal
		rel, err := filepath.Rel(paths.Unpacked, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("tar path escapes unpack dir: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", target, err)
			}
			out, err := os.Create(target)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			out.Close()
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("tar contains unsupported link entry: %s", hdr.Name)
		}
	}
	return nil
}

// ValidateEntrypoint checks that the unpacked bundle contains an app.R file
// at the top level. In v0, all deployments must have this entrypoint.
func ValidateEntrypoint(paths Paths) error {
	entrypoint := filepath.Join(paths.Unpacked, "app.R")
	if _, err := os.Stat(entrypoint); os.IsNotExist(err) {
		return fmt.Errorf("bundle must contain an app.R entrypoint")
	} else if err != nil {
		return fmt.Errorf("check entrypoint: %w", err)
	}
	return nil
}

// CreateLibraryDir creates the output directory for dependency restoration.
func CreateLibraryDir(paths Paths) error {
	return os.MkdirAll(paths.Library, 0o755)
}

// SetLibraryPath sets the library key in the bundle's rproject.toml to the
// given container-side path. This is called after unpacking so the build
// container writes restored packages to the mounted library volume rather
// than to a path relative to the read-only /app mount.
func SetLibraryPath(paths Paths, containerLibPath string) error {
	configPath := filepath.Join(paths.Unpacked, "rproject.toml")

	var cfg map[string]any
	if _, err := toml.DecodeFile(configPath, &cfg); err != nil {
		if os.IsNotExist(err) {
			return nil // no config file, nothing to do
		}
		return fmt.Errorf("parse rproject.toml: %w", err)
	}

	cfg["library"] = containerLibPath

	f, err := os.Create(configPath)
	if err != nil {
		return fmt.Errorf("write rproject.toml: %w", err)
	}
	defer f.Close()

	if err := toml.NewEncoder(f).Encode(cfg); err != nil {
		return fmt.Errorf("encode rproject.toml: %w", err)
	}

	return nil
}

// DeleteFiles removes a bundle's archive, unpacked dir, and library dir.
// Errors are logged but do not fail the operation.
func DeleteFiles(paths Paths) {
	for _, p := range []string{paths.Archive, paths.Unpacked, paths.Library} {
		if err := os.RemoveAll(p); err != nil {
			slog.Warn("failed to delete bundle path", "path", p, "error", err)
		}
	}
}

// EnforceRetention deletes the oldest non-active bundles when the count
// exceeds retention. Returns IDs of deleted bundles.
func EnforceRetention(database *db.DB, base, appID string, activeBundleID string, retention int) []string {
	bundles, err := database.ListBundlesByApp(appID)
	if err != nil {
		slog.Warn("retention: list bundles failed", "app_id", appID, "error", err)
		return nil
	}

	// Bundles are ordered newest-first. Keep the first `retention` plus
	// any bundle that is the active one.
	var toDelete []db.BundleRow
	kept := 0
	for _, b := range bundles {
		isActive := b.ID == activeBundleID
		if isActive || kept < retention {
			if !isActive {
				kept++
			}
			continue
		}
		toDelete = append(toDelete, b)
	}

	var deletedIDs []string
	for _, b := range toDelete {
		paths := NewBundlePaths(base, appID, b.ID)
		DeleteFiles(paths)
		if _, err := database.DeleteBundle(b.ID); err != nil {
			slog.Warn("retention: delete bundle row failed",
				"bundle_id", b.ID, "error", err)
		} else {
			deletedIDs = append(deletedIDs, b.ID)
		}
	}
	return deletedIDs
}
