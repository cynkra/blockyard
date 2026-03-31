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

	"github.com/cynkra/blockyard/internal/db"
)

// Paths holds the filesystem locations for a bundle.
type Paths struct {
	Base     string // {base}/{app_id}/bundles/{bundle_id}/ — parent for bundle artifacts
	Archive  string // {base}/{app_id}/{bundle_id}.tar.gz
	Unpacked string // {base}/{app_id}/{bundle_id}/
	Library  string // {base}/{app_id}/{bundle_id}_lib/
}

// NewBundlePaths constructs paths for a bundle. Single source of truth for
// the on-disk layout.
func NewBundlePaths(base, appID, bundleID string) Paths {
	appDir := filepath.Join(base, appID)
	return Paths{
		Base:     appDir,
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

// maxDecompressedSize is the upper bound on total bytes extracted from a
// bundle archive. This prevents gzip/tar bombs where a small compressed
// file expands to exhaust disk space. Set to 2 GiB.
const maxDecompressedSize int64 = 2 << 30

// UnpackArchive decompresses the tar.gz archive into the unpacked directory.
// Extraction is capped at maxDecompressedSize total bytes to prevent
// gzip/tar bomb attacks.
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
	var totalWritten int64
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
			remaining := maxDecompressedSize - totalWritten
			n, copyErr := io.Copy(out, io.LimitReader(tr, remaining+1))
			closeErr := out.Close()
			totalWritten += n
			if copyErr != nil {
				return fmt.Errorf("write %s: %w", target, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close %s: %w", target, closeErr)
			}
			if totalWritten > maxDecompressedSize {
				return fmt.Errorf("decompressed content exceeds %d byte limit", maxDecompressedSize)
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("tar contains unsupported link entry: %s", hdr.Name)
		}
	}
	return nil
}

// ValidateEntrypoint checks that the unpacked bundle contains an app.R
// or server.R file at the top level.
func ValidateEntrypoint(paths Paths) error {
	for _, name := range []string{"app.R", "server.R"} {
		if _, err := os.Stat(filepath.Join(paths.Unpacked, name)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("missing entrypoint: app.R or server.R")
}

// CreateLibraryDir creates the output directory for dependency restoration.
func CreateLibraryDir(paths Paths) error {
	return os.MkdirAll(paths.Library, 0o755)
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
