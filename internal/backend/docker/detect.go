package docker

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/moby/moby/api/types/container"
)

// detectMountMode examines the server container's mount set to determine
// how dataPath (e.g. "/data/bundles") is mounted. It returns a MountConfig
// describing the mount type (native, bind, or volume) and the information
// needed to create corresponding mounts for sibling containers.
//
// Pass nil mounts for native mode (no containerized server). The caller is
// responsible for inspecting the server container and passing its mounts;
// this keeps detection a pure function and lets the caller reuse the
// inspect result for other purposes (e.g. canonicalizing the container ID).
//
// Detection algorithm:
//  1. Iterate the server container's mounts.
//  2. Find the mount whose Destination is the longest prefix of dataPath.
//  3. If type=volume → store volume name and destination.
//  4. If type=bind → store host source path and destination.
//  5. If no covering mount is found → error.
func detectMountMode(mounts []container.MountPoint, dataPath string) (MountConfig, error) {
	if mounts == nil {
		slog.Info("mount auto-detect: native mode (no container)")
		return MountConfig{Mode: MountModeNative}, nil
	}

	// Find the mount whose Destination is the longest prefix of dataPath.
	var bestDest string
	var bestCfg MountConfig
	found := false

	for _, m := range mounts {
		dest := m.Destination
		if !pathIsPrefix(dest, dataPath) {
			continue
		}
		// Prefer the longest (most specific) prefix match.
		if len(dest) <= len(bestDest) {
			continue
		}

		switch m.Type {
		case "volume":
			bestCfg = MountConfig{
				Mode:       MountModeVolume,
				VolumeName: m.Name,
				MountDest:  dest,
			}
			bestDest = dest
			found = true
		case "bind":
			bestCfg = MountConfig{
				Mode:       MountModeBind,
				HostSource: m.Source,
				MountDest:  dest,
			}
			bestDest = dest
			found = true
		default:
			slog.Warn("mount auto-detect: unsupported mount type",
				"type", m.Type, "destination", dest)
		}
	}

	if !found {
		return MountConfig{}, fmt.Errorf(
			"mount auto-detect: data path %q is not on a persistent mount; "+
				"mount a volume or bind mount at the data directory", dataPath)
	}

	switch bestCfg.Mode {
	case MountModeVolume:
		slog.Info("mount auto-detect: volume mode",
			"volume", bestCfg.VolumeName, "destination", bestCfg.MountDest)
	case MountModeBind:
		slog.Info("mount auto-detect: bind mode",
			"source", bestCfg.HostSource, "destination", bestCfg.MountDest)
	}

	return bestCfg, nil
}

// pathIsPrefix reports whether prefix is a path prefix of target.
// It handles trailing slashes and ensures "/data" matches "/data/bundles"
// but not "/data2".
func pathIsPrefix(prefix, target string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	target = strings.TrimSuffix(target, "/")
	if prefix == target {
		return true
	}
	return strings.HasPrefix(target, prefix+"/")
}
