package docker

import (
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/mount"

	"github.com/cynkra/blockyard/internal/bundle"
)

// MountConfig holds the settings needed to decide between bind-mount and
// named-volume mode when creating worker/build containers.
type MountConfig struct {
	VolumeName     string // if non-empty, use named Docker volume mode
	BundleBasePath string // server-side base path, used to derive volume subpaths
}

// UseVolume reports whether named volume mode is active.
func (mc MountConfig) UseVolume() bool {
	return mc.VolumeName != ""
}

// subpath strips BundleBasePath from an absolute server-side path to produce
// the volume subpath. For example, given base "/data/bundles" and path
// "/data/bundles/app1/abc123", it returns "app1/abc123".
func (mc MountConfig) subpath(serverPath string) string {
	rel := strings.TrimPrefix(serverPath, mc.BundleBasePath)
	rel = strings.TrimPrefix(rel, string(filepath.Separator))
	return rel
}

// volumeMount creates a mount.Mount of type Volume with an optional subpath.
func (mc MountConfig) volumeMount(target string, readOnly bool, serverPath string) mount.Mount {
	m := mount.Mount{
		Type:     mount.TypeVolume,
		Source:   mc.VolumeName,
		Target:   target,
		ReadOnly: readOnly,
		VolumeOptions: &mount.VolumeOptions{
			Subpath: mc.subpath(serverPath),
		},
	}
	return m
}

// WorkerMounts returns the container HostConfig fields for a worker container.
// In bind mode, only Binds is populated. In volume mode, only Mounts is populated.
func (mc MountConfig) WorkerMounts(bundlePath, libraryPath, workerMount string) (binds []string, mounts []mount.Mount) {
	if !mc.UseVolume() {
		binds = []string{
			bundlePath + ":" + workerMount + ":ro",
		}
		if libraryPath != "" {
			binds = append(binds, libraryPath+":/blockyard-lib:ro")
		}
		return binds, nil
	}

	mounts = []mount.Mount{
		mc.volumeMount(workerMount, true, bundlePath),
	}
	if libraryPath != "" {
		mounts = append(mounts, mc.volumeMount("/blockyard-lib", true, libraryPath))
	}
	return nil, mounts
}

// BuildMounts returns the container HostConfig fields for a build container.
// In bind mode, only Binds is populated. In volume mode, only Mounts is populated.
func (mc MountConfig) BuildMounts(bundlePath, libraryPath, rvBinaryPath string) (binds []string, mounts []mount.Mount) {
	if !mc.UseVolume() {
		binds = []string{
			bundlePath + ":/app:ro",
			libraryPath + ":" + bundle.BuildContainerLibPath,
			rvBinaryPath + ":/usr/local/bin/rv:ro",
		}
		return binds, nil
	}

	mounts = []mount.Mount{
		mc.volumeMount("/app", true, bundlePath),
		mc.volumeMount(bundle.BuildContainerLibPath, false, libraryPath),
		mc.volumeMount("/usr/local/bin/rv", true, rvBinaryPath),
	}
	return nil, mounts
}
