package process

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// rigBase is the directory where rig installs R versions.
// Tests override this via setRigBase.
var rigBase = "/opt/R" //nolint:gochecknoglobals // test seam

func setRigBase(path string) { rigBase = path }

// ResolveRBinary maps a requested R version (e.g. "4.5.0") to the
// full path of a rig-managed R binary. Resolution order:
//
//  1. Exact match: /opt/R/<version>/bin/R
//  2. Minor match: highest /opt/R/<major>.<minor>.*/bin/R
//  3. Fallback: the configured default R path
//
// Returns the resolved path and whether the fallback was used. When
// version is empty the fallback is returned directly (not considered
// a miss).
func ResolveRBinary(version, fallback string) (string, bool) {
	if version == "" {
		return fallback, false
	}

	// 1. Exact match.
	path := filepath.Join(rigBase, version, "bin", "R")
	if _, err := os.Stat(path); err == nil {
		return path, false
	}

	// 2. Minor match — if version is X.Y.Z, glob X.Y.* and pick
	// the highest patch release.
	parts := strings.SplitN(version, ".", 3)
	if len(parts) >= 2 {
		prefix := parts[0] + "." + parts[1]

		pattern := filepath.Join(rigBase, prefix+".*", "bin", "R")
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			sort.Strings(matches)
			return matches[len(matches)-1], false
		}

		// Try bare major.minor (e.g. /opt/R/4.5/bin/R).
		path = filepath.Join(rigBase, prefix, "bin", "R")
		if _, err := os.Stat(path); err == nil {
			return path, false
		}
	}

	// 3. Fallback.
	return fallback, true
}

// InstalledRVersions returns the R versions installed under rigBase.
// Each entry is the directory name (e.g. "4.5.0", "4.4.3").
func InstalledRVersions() []string {
	matches, err := filepath.Glob(filepath.Join(rigBase, "*", "bin", "R"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	versions := make([]string, 0, len(matches))
	for _, m := range matches {
		// /opt/R/<version>/bin/R → extract <version>
		dir := filepath.Dir(filepath.Dir(m))
		versions = append(versions, filepath.Base(dir))
	}
	sort.Strings(versions)
	return versions
}
