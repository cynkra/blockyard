package docker

import (
	"testing"

	"github.com/docker/docker/api/types/mount"
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
		got := mc.toHostPath(tt.input)
		if got != tt.want {
			t.Errorf("toHostPath(%q) = %q, want %q", tt.input, got, tt.want)
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
