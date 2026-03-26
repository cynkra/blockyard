package pkgstore

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
