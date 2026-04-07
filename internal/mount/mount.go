package mount

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
)

// ReservedPaths are container paths that mount targets cannot collide
// with. Checked by prefix — "/app/foo" collides with "/app".
var ReservedPaths = []string{
	"/app",
	"/tmp",
	"/blockyard-lib",
	"/blockyard-lib-store",
	"/transfer",
	"/var/run/blockyard",
}

// Validate checks a list of data mount rows against the admin-defined
// mount sources. Returns the first validation error found.
func Validate(mounts []db.DataMountRow, sources []config.DataMountSource) error {
	sourceMap := make(map[string]string, len(sources))
	for _, s := range sources {
		sourceMap[s.Name] = s.Path
	}

	targets := make(map[string]bool)

	for i, m := range mounts {
		// Source must reference a known admin-defined mount.
		baseName := m.Source
		subpath := ""
		if idx := strings.IndexByte(m.Source, '/'); idx >= 0 {
			baseName = m.Source[:idx]
			subpath = m.Source[idx+1:]
		}

		if _, ok := sourceMap[baseName]; !ok {
			return fmt.Errorf("mount[%d]: unknown source %q", i, baseName)
		}

		// No path traversal in source subpath.
		if strings.Contains(subpath, "..") {
			return fmt.Errorf("mount[%d]: source subpath must not contain \"..\"", i)
		}

		// Target must be absolute.
		if !filepath.IsAbs(m.Target) {
			return fmt.Errorf("mount[%d]: target %q must be absolute", i, m.Target)
		}

		// No path traversal in target.
		if strings.Contains(m.Target, "..") {
			return fmt.Errorf("mount[%d]: target must not contain \"..\"", i)
		}

		// Target must not collide with reserved paths.
		cleanTarget := filepath.Clean(m.Target)
		for _, reserved := range ReservedPaths {
			if cleanTarget == reserved || strings.HasPrefix(cleanTarget, reserved+"/") {
				return fmt.Errorf("mount[%d]: target %q collides with reserved path %q",
					i, m.Target, reserved)
			}
		}

		// No duplicate targets.
		if targets[cleanTarget] {
			return fmt.Errorf("mount[%d]: duplicate target %q", i, m.Target)
		}
		targets[cleanTarget] = true
	}

	return nil
}

// Resolve converts validated data mount rows into MountEntries with
// host paths as Source. The returned entries are ready to be passed
// directly to the Docker API as bind-mount sources — no MountConfig
// translation needed. Returns an error if a source references a name
// that no longer exists in the admin config.
func Resolve(mounts []db.DataMountRow, sources []config.DataMountSource) ([]backend.MountEntry, error) {
	sourceMap := make(map[string]string, len(sources))
	for _, s := range sources {
		sourceMap[s.Name] = s.Path
	}

	entries := make([]backend.MountEntry, 0, len(mounts))
	for _, m := range mounts {
		baseName := m.Source
		subpath := ""
		if idx := strings.IndexByte(m.Source, '/'); idx >= 0 {
			baseName = m.Source[:idx]
			subpath = m.Source[idx+1:]
		}

		hostPath, ok := sourceMap[baseName]
		if !ok {
			return nil, fmt.Errorf("mount source %q not found in config", baseName)
		}
		if subpath != "" {
			hostPath = filepath.Join(hostPath, subpath)
		}

		entries = append(entries, backend.MountEntry{
			Source:   hostPath,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return entries, nil
}
