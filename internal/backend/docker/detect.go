package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/docker/docker/client"
)

// detectMountMode inspects the server's own container to determine how
// dataPath (e.g. "/data/bundles") is mounted. It returns a MountConfig
// describing the mount type (native, bind, or volume) and the information
// needed to create corresponding mounts for sibling containers.
//
// Detection algorithm:
//  1. If serverID is empty (native mode), no translation is needed.
//  2. Inspect the server container and iterate its mounts.
//  3. Find the mount whose Destination is a prefix of dataPath.
//  4. If type=volume → store volume name and destination.
//  5. If type=bind → store host source path and destination.
//  6. If no covering mount is found → error.
func detectMountMode(ctx context.Context, cli *client.Client, serverID, dataPath string) (MountConfig, error) {
	if serverID == "" {
		slog.Info("mount auto-detect: native mode (no container)")
		return MountConfig{Mode: MountModeNative}, nil
	}

	info, err := cli.ContainerInspect(ctx, serverID)
	if err != nil {
		return MountConfig{}, fmt.Errorf("mount auto-detect: inspect container %s: %w", serverID, err)
	}

	// Find the mount whose Destination is the longest prefix of dataPath.
	var bestDest string
	var bestCfg MountConfig
	found := false

	for _, m := range info.Mounts {
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
