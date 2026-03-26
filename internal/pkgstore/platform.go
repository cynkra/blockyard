package pkgstore

import (
	"log/slog"
	"os"
	"strings"
)

// RecoverPlatform scans the store root for existing platform
// directories (e.g., "4.5-x86_64-pc-linux-gnu/") to restore the
// platform after a server restart.
func RecoverPlatform(storePath string) string {
	entries, err := os.ReadDir(storePath)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		// Platform dirs look like "4.5-x86_64-pc-linux-gnu".
		if strings.Contains(e.Name(), "-") {
			slog.Info("recovered store platform", "platform", e.Name())
			return e.Name()
		}
	}
	return ""
}

// PlatformFromLockfile derives the platform prefix from a pak lockfile.
// Uses per-package fields (short form) rather than top-level fields
// (which contain long human-readable strings like
// "R version 4.5.2 (2025-10-31)").
func PlatformFromLockfile(lf *Lockfile) string {
	if len(lf.Packages) == 0 {
		return ""
	}
	pkg := lf.Packages[0]
	return pkg.RVersion + "-" + pkg.Platform
}
