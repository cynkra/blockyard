package docker

import (
	"testing"

	"github.com/moby/moby/api/types/mount"

	"github.com/cynkra/blockyard/internal/backend"
)

func TestMountConfig_WorkerMounts_NativeMode(t *testing.T) {
	mc := MountConfig{Mode: MountModeNative}

	binds, mounts := mc.WorkerMounts("/data/bundles/app1/b1", "/data/bundles/app1/b1_lib", "", "", "", "/app")
	if len(mounts) != 0 {
		t.Fatalf("expected no mounts in native mode, got %d", len(mounts))
	}
	if len(binds) != 2 {
		t.Fatalf("expected 2 binds, got %d: %v", len(binds), binds)
	}
	if binds[0] != "/data/bundles/app1/b1:/app:ro" {
		t.Errorf("bind[0] = %q", binds[0])
	}
	if binds[1] != "/data/bundles/app1/b1_lib:/blockyard-lib:ro" {
		t.Errorf("bind[1] = %q", binds[1])
	}
}

func TestMountConfig_WorkerMounts_NativeMode_NoLibrary(t *testing.T) {
	mc := MountConfig{Mode: MountModeNative}
	binds, mounts := mc.WorkerMounts("/data/bundles/app1/b1", "", "", "", "", "/app")
	if len(mounts) != 0 {
		t.Fatalf("expected no mounts, got %d", len(mounts))
	}
	if len(binds) != 1 {
		t.Fatalf("expected 1 bind, got %d", len(binds))
	}
}

func TestMountConfig_WorkerMounts_BindMode(t *testing.T) {
	mc := MountConfig{
		Mode:       MountModeBind,
		HostSource: "/host/path/data",
		MountDest:  "/data",
	}

	binds, mounts := mc.WorkerMounts("/data/bundles/app1/b1", "/data/bundles/app1/b1_lib", "", "", "", "/app")
	if len(mounts) != 0 {
		t.Fatalf("expected no mounts in bind mode, got %d", len(mounts))
	}
	if len(binds) != 2 {
		t.Fatalf("expected 2 binds, got %d: %v", len(binds), binds)
	}
	if binds[0] != "/host/path/data/bundles/app1/b1:/app:ro" {
		t.Errorf("bind[0] = %q", binds[0])
	}
	if binds[1] != "/host/path/data/bundles/app1/b1_lib:/blockyard-lib:ro" {
		t.Errorf("bind[1] = %q", binds[1])
	}
}

func TestMountConfig_WorkerMounts_VolumeMode(t *testing.T) {
	mc := MountConfig{
		Mode:       MountModeVolume,
		VolumeName: "blockyard-data",
		MountDest:  "/data",
	}

	binds, mounts := mc.WorkerMounts("/data/bundles/app1/b1", "/data/bundles/app1/b1_lib", "", "", "", "/app")
	if len(binds) != 0 {
		t.Fatalf("expected no binds in volume mode, got %d", len(binds))
	}
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}

	assertVolumeMount(t, mounts[0], "blockyard-data", "/app", true, "bundles/app1/b1")
	assertVolumeMount(t, mounts[1], "blockyard-data", "/blockyard-lib", true, "bundles/app1/b1_lib")
}

func TestMountConfig_Subpath(t *testing.T) {
	mc := MountConfig{
		Mode:       MountModeVolume,
		VolumeName: "vol",
		MountDest:  "/data",
	}

	tests := []struct {
		input string
		want  string
	}{
		{"/data/bundles/app1/b1", "bundles/app1/b1"},
		{"/data/bundles/.pak-cache/pak-stable", "bundles/.pak-cache/pak-stable"},
		{"/data", ""},
	}
	for _, tt := range tests {
		got := mc.subpath(tt.input)
		if got != tt.want {
			t.Errorf("subpath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMountConfig_ToHostPath(t *testing.T) {
	mc := MountConfig{
		Mode:       MountModeBind,
		HostSource: "/host/path/data",
		MountDest:  "/data",
	}

	tests := []struct {
		input string
		want  string
	}{
		{"/data/bundles/app1/b1", "/host/path/data/bundles/app1/b1"},
		{"/data/bundles/.pak-cache/pak-stable", "/host/path/data/bundles/.pak-cache/pak-stable"},
		{"/data", "/host/path/data"},
	}
	for _, tt := range tests {
		got := mc.ToHostPath(tt.input)
		if got != tt.want {
			t.Errorf("ToHostPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPathIsPrefix(t *testing.T) {
	tests := []struct {
		prefix, target string
		want           bool
	}{
		{"/data", "/data/bundles", true},
		{"/data", "/data", true},
		{"/data", "/data2", false},
		{"/data/", "/data/bundles", true},
		{"/foo", "/data/bundles", false},
	}
	for _, tt := range tests {
		got := pathIsPrefix(tt.prefix, tt.target)
		if got != tt.want {
			t.Errorf("pathIsPrefix(%q, %q) = %v, want %v", tt.prefix, tt.target, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// TranslateMount
// ---------------------------------------------------------------------------

func TestTranslateMount_NativeMode(t *testing.T) {
	mc := MountConfig{Mode: MountModeNative}

	binds, mounts := mc.TranslateMount(backend.MountEntry{
		Source: "/data/extra", Target: "/extra", ReadOnly: true,
	})
	if len(mounts) != 0 {
		t.Fatalf("expected no mounts in native mode, got %d", len(mounts))
	}
	if len(binds) != 1 || binds[0] != "/data/extra:/extra:ro" {
		t.Errorf("binds = %v", binds)
	}

	binds, _ = mc.TranslateMount(backend.MountEntry{
		Source: "/data/rw", Target: "/rw", ReadOnly: false,
	})
	if len(binds) != 1 || binds[0] != "/data/rw:/rw" {
		t.Errorf("rw binds = %v", binds)
	}
}

func TestTranslateMount_BindMode(t *testing.T) {
	mc := MountConfig{
		Mode:       MountModeBind,
		HostSource: "/host/data",
		MountDest:  "/data",
	}

	binds, mounts := mc.TranslateMount(backend.MountEntry{
		Source: "/data/extra", Target: "/extra", ReadOnly: true,
	})
	if len(mounts) != 0 {
		t.Fatalf("expected no mounts in bind mode, got %d", len(mounts))
	}
	if len(binds) != 1 || binds[0] != "/host/data/extra:/extra:ro" {
		t.Errorf("binds = %v", binds)
	}

	binds, _ = mc.TranslateMount(backend.MountEntry{
		Source: "/data/rw", Target: "/rw", ReadOnly: false,
	})
	if len(binds) != 1 || binds[0] != "/host/data/rw:/rw" {
		t.Errorf("rw binds = %v", binds)
	}
}

func TestTranslateMount_VolumeMode(t *testing.T) {
	mc := MountConfig{
		Mode:       MountModeVolume,
		VolumeName: "blockyard-data",
		MountDest:  "/data",
	}

	binds, mounts := mc.TranslateMount(backend.MountEntry{
		Source: "/data/extra", Target: "/extra", ReadOnly: true,
	})
	if len(binds) != 0 {
		t.Fatalf("expected no binds in volume mode, got %d", len(binds))
	}
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	assertVolumeMount(t, mounts[0], "blockyard-data", "/extra", true, "extra")
}

// ---------------------------------------------------------------------------
// WorkerMounts — expanded parameter coverage
// ---------------------------------------------------------------------------

func TestMountConfig_WorkerMounts_NativeMode_WithTransferAndToken(t *testing.T) {
	mc := MountConfig{Mode: MountModeNative}

	binds, mounts := mc.WorkerMounts(
		"/data/bundles/app1/b1",
		"/data/bundles/app1/b1_lib",
		"",
		"/data/transfer/app1",
		"/data/tokens/app1",
		"/app",
	)
	if len(mounts) != 0 {
		t.Fatalf("expected no mounts in native mode, got %d", len(mounts))
	}
	if len(binds) != 4 {
		t.Fatalf("expected 4 binds, got %d: %v", len(binds), binds)
	}
	if binds[2] != "/data/transfer/app1:/transfer" {
		t.Errorf("transfer bind = %q", binds[2])
	}
	if binds[3] != "/data/tokens/app1:/var/run/blockyard:ro" {
		t.Errorf("token bind = %q", binds[3])
	}
}

func TestMountConfig_WorkerMounts_BindMode_WithTransferAndToken(t *testing.T) {
	mc := MountConfig{
		Mode:       MountModeBind,
		HostSource: "/host/data",
		MountDest:  "/data",
	}

	binds, mounts := mc.WorkerMounts(
		"/data/bundles/app1/b1",
		"/data/bundles/app1/b1_lib",
		"",
		"/data/transfer/app1",
		"/data/tokens/app1",
		"/app",
	)
	if len(mounts) != 0 {
		t.Fatalf("expected no mounts in bind mode, got %d", len(mounts))
	}
	if len(binds) != 4 {
		t.Fatalf("expected 4 binds, got %d: %v", len(binds), binds)
	}
	if binds[2] != "/host/data/transfer/app1:/transfer" {
		t.Errorf("transfer bind = %q", binds[2])
	}
	if binds[3] != "/host/data/tokens/app1:/var/run/blockyard:ro" {
		t.Errorf("token bind = %q", binds[3])
	}
}

func TestMountConfig_WorkerMounts_VolumeMode_WithTransferAndToken(t *testing.T) {
	mc := MountConfig{
		Mode:       MountModeVolume,
		VolumeName: "blockyard-data",
		MountDest:  "/data",
	}

	binds, mounts := mc.WorkerMounts(
		"/data/bundles/app1/b1",
		"/data/bundles/app1/b1_lib",
		"",
		"/data/transfer/app1",
		"/data/tokens/app1",
		"/app",
	)
	if len(binds) != 0 {
		t.Fatalf("expected no binds in volume mode, got %d", len(binds))
	}
	if len(mounts) != 4 {
		t.Fatalf("expected 4 mounts, got %d", len(mounts))
	}
	assertVolumeMount(t, mounts[2], "blockyard-data", "/transfer", false, "transfer/app1")
	assertVolumeMount(t, mounts[3], "blockyard-data", "/var/run/blockyard", true, "tokens/app1")
}

func TestMountConfig_WorkerMounts_WithLibDir(t *testing.T) {
	mc := MountConfig{Mode: MountModeNative}

	binds, _ := mc.WorkerMounts(
		"/data/bundles/app1/b1",
		"",
		"/data/lib-store/app1",
		"",
		"",
		"/app",
	)
	if len(binds) != 2 {
		t.Fatalf("expected 2 binds, got %d: %v", len(binds), binds)
	}
	// libDir uses /blockyard-lib-store mount point.
	if binds[1] != "/data/lib-store/app1:/blockyard-lib-store:ro" {
		t.Errorf("lib bind = %q, want libDir mounted at /blockyard-lib-store", binds[1])
	}
}

func TestMountConfig_WorkerMounts_LibDirOverridesLibraryPath(t *testing.T) {
	mc := MountConfig{Mode: MountModeNative}

	// When both libDir and libraryPath are set, libDir wins.
	binds, _ := mc.WorkerMounts(
		"/data/bundles/app1/b1",
		"/data/bundles/app1/b1_lib",
		"/data/lib-store/app1",
		"",
		"",
		"/app",
	)
	if len(binds) != 2 {
		t.Fatalf("expected 2 binds, got %d: %v", len(binds), binds)
	}
	if binds[1] != "/data/lib-store/app1:/blockyard-lib-store:ro" {
		t.Errorf("lib bind = %q, libDir should take precedence", binds[1])
	}
}

func TestTranslateMount_RProfile_NativeMode(t *testing.T) {
	mc := MountConfig{Mode: MountModeNative}
	binds, mounts := mc.TranslateMount(backend.MountEntry{
		Source: "/data/bundles/.blockyard-rprofile.R", Target: "/etc/blockyard/rprofile.R", ReadOnly: true,
	})
	if len(mounts) != 0 {
		t.Fatalf("expected no volume mounts in native mode, got %d", len(mounts))
	}
	if len(binds) != 1 {
		t.Fatalf("expected 1 bind, got %d", len(binds))
	}
	if binds[0] != "/data/bundles/.blockyard-rprofile.R:/etc/blockyard/rprofile.R:ro" {
		t.Errorf("bind = %q", binds[0])
	}
}

func TestTranslateMount_RProfile_BindMode(t *testing.T) {
	mc := MountConfig{
		Mode:       MountModeBind,
		HostSource: "/host/data",
		MountDest:  "/data",
	}
	binds, mounts := mc.TranslateMount(backend.MountEntry{
		Source: "/data/bundles/.blockyard-rprofile.R", Target: "/etc/blockyard/rprofile.R", ReadOnly: true,
	})
	if len(mounts) != 0 {
		t.Fatalf("expected no volume mounts in bind mode, got %d", len(mounts))
	}
	if len(binds) != 1 {
		t.Fatalf("expected 1 bind, got %d", len(binds))
	}
	if binds[0] != "/host/data/bundles/.blockyard-rprofile.R:/etc/blockyard/rprofile.R:ro" {
		t.Errorf("bind = %q, expected host-translated path", binds[0])
	}
}

func TestTranslateMount_RProfile_VolumeMode(t *testing.T) {
	mc := MountConfig{
		Mode:       MountModeVolume,
		VolumeName: "blockyard-data",
		MountDest:  "/data",
	}
	binds, mounts := mc.TranslateMount(backend.MountEntry{
		Source: "/data/bundles/.blockyard-rprofile.R", Target: "/etc/blockyard/rprofile.R", ReadOnly: true,
	})
	if len(binds) != 0 {
		t.Fatalf("expected no binds in volume mode, got %d", len(binds))
	}
	if len(mounts) != 1 {
		t.Fatalf("expected 1 volume mount, got %d", len(mounts))
	}
	assertVolumeMount(t, mounts[0], "blockyard-data", "/etc/blockyard/rprofile.R", true, "bundles/.blockyard-rprofile.R")
}

func assertVolumeMount(t *testing.T, m mount.Mount, source, target string, readOnly bool, subpath string) {
	t.Helper()
	if m.Type != mount.TypeVolume {
		t.Errorf("expected TypeVolume, got %q", m.Type)
	}
	if m.Source != source {
		t.Errorf("source = %q, want %q", m.Source, source)
	}
	if m.Target != target {
		t.Errorf("target = %q, want %q", m.Target, target)
	}
	if m.ReadOnly != readOnly {
		t.Errorf("readOnly = %v, want %v", m.ReadOnly, readOnly)
	}
	if m.VolumeOptions == nil {
		t.Fatal("VolumeOptions is nil")
	}
	if m.VolumeOptions.Subpath != subpath {
		t.Errorf("subpath = %q, want %q", m.VolumeOptions.Subpath, subpath)
	}
}
