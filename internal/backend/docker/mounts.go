package docker

import (
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/mount"

	"github.com/cynkra/blockyard/internal/backend"
)

// MountMode describes how the server's data directory is mounted.
type MountMode int

const (
	// MountModeNative means no container detected; server path = host path.
	MountModeNative MountMode = iota
	// MountModeBind means the data path is a bind mount; paths are translated
	// by replacing the container-side prefix with the host-side source.
	MountModeBind
	// MountModeVolume means the data path is on a named Docker volume;
	// sibling containers use volume subpath mounts.
	MountModeVolume
)

// MountConfig holds the auto-detected mount information used to create
// bind mounts or volume mounts for sibling containers.
type MountConfig struct {
	Mode       MountMode
	VolumeName string // MountModeVolume only: name of the Docker volume
	HostSource string // MountModeBind only: host-side path of the mount source
	MountDest  string // container-side mount destination (e.g. "/data")
}

// subpath strips MountDest from an absolute server-side path to produce
// the volume subpath. For example, given MountDest "/data" and path
// "/data/bundles/app1/abc123", it returns "bundles/app1/abc123".
func (mc MountConfig) subpath(serverPath string) string {
	rel := strings.TrimPrefix(serverPath, mc.MountDest)
	rel = strings.TrimPrefix(rel, string(filepath.Separator))
	return rel
}

// toHostPath translates a server-side path to the corresponding host path
// by replacing the MountDest prefix with HostSource.
func (mc MountConfig) toHostPath(serverPath string) string {
	rel := mc.subpath(serverPath)
	if rel == "" {
		return mc.HostSource
	}
	return filepath.Join(mc.HostSource, rel)
}

// volumeMount creates a mount.Mount of type Volume with an optional subpath.
func (mc MountConfig) volumeMount(target string, readOnly bool, serverPath string) mount.Mount {
	return mount.Mount{
		Type:     mount.TypeVolume,
		Source:   mc.VolumeName,
		Target:   target,
		ReadOnly: readOnly,
		VolumeOptions: &mount.VolumeOptions{
			Subpath: mc.subpath(serverPath),
		},
	}
}

// WorkerMounts returns the container HostConfig fields for a worker container.
// All paths are server-side; MountConfig translates them as needed.
//
// libDir is the store-assembled per-worker library (phase 2-6); mounted ro
// at /lib. Host-side writes (runtime package hardlinks by the server) are
// visible through the bind mount regardless of the ro flag.
//
// transferDir is pre-created for container transfer signaling (phase 2-7);
// mounted rw at /transfer.
//
// libraryPath is the legacy per-bundle library from phase 2-5; used when
// libDir is empty (pre-store bundles).
func (mc MountConfig) WorkerMounts(bundlePath, libraryPath, libDir, transferDir, tokenDir, workerMount string) (binds []string, mounts []mount.Mount) {
	// Choose the library path: prefer store-assembled libDir over legacy libraryPath.
	effectiveLib := libDir
	libMount := "/blockyard-lib-store"
	if effectiveLib == "" {
		effectiveLib = libraryPath
		libMount = "/blockyard-lib"
	}

	if mc.Mode == MountModeVolume {
		mounts = []mount.Mount{
			mc.volumeMount(workerMount, true, bundlePath),
		}
		if effectiveLib != "" {
			mounts = append(mounts, mc.volumeMount(libMount, true, effectiveLib))
		}
		if transferDir != "" {
			mounts = append(mounts, mc.volumeMount("/transfer", false, transferDir))
		}
		if tokenDir != "" {
			mounts = append(mounts, mc.volumeMount("/var/run/blockyard", true, tokenDir))
		}
		return nil, mounts
	}

	// Native or Bind mode — use bind mounts.
	bp := bundlePath
	lp := effectiveLib
	tp := transferDir
	tkp := tokenDir
	if mc.Mode == MountModeBind {
		bp = mc.toHostPath(bundlePath)
		if lp != "" {
			lp = mc.toHostPath(lp)
		}
		if tp != "" {
			tp = mc.toHostPath(tp)
		}
		if tkp != "" {
			tkp = mc.toHostPath(tkp)
		}
	}

	binds = []string{
		bp + ":" + workerMount + ":ro",
	}
	if lp != "" {
		binds = append(binds, lp+":"+libMount+":ro")
	}
	if tp != "" {
		binds = append(binds, tp+":/transfer")
	}
	if tkp != "" {
		binds = append(binds, tkp+":/var/run/blockyard:ro")
	}
	return binds, nil
}

// TranslateMount converts a single backend.MountEntry into the appropriate
// Docker bind or volume mount for the detected mount mode.
func (mc MountConfig) TranslateMount(m backend.MountEntry) (
	binds []string, mounts []mount.Mount,
) {
	switch mc.Mode {
	case MountModeVolume:
		mounts = append(mounts,
			mc.volumeMount(m.Target, m.ReadOnly, m.Source))
	case MountModeBind:
		flag := ":ro"
		if !m.ReadOnly {
			flag = ""
		}
		binds = append(binds, mc.toHostPath(m.Source)+":"+m.Target+flag)
	default: // Native
		flag := ":ro"
		if !m.ReadOnly {
			flag = ""
		}
		binds = append(binds, m.Source+":"+m.Target+flag)
	}
	return binds, mounts
}

