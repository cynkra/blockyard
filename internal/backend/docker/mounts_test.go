package docker

import (
	"testing"

	"github.com/docker/docker/api/types/mount"

	"github.com/cynkra/blockyard/internal/bundle"
)

func TestMountConfig_WorkerMounts_BindMode(t *testing.T) {
	mc := MountConfig{} // empty VolumeName = bind mode

	binds, mounts := mc.WorkerMounts("/host/bundles/app1/b1", "/host/bundles/app1/b1_lib", "/app")
	if len(mounts) != 0 {
		t.Fatalf("expected no mounts in bind mode, got %d", len(mounts))
	}
	if len(binds) != 2 {
		t.Fatalf("expected 2 binds, got %d: %v", len(binds), binds)
	}
	if binds[0] != "/host/bundles/app1/b1:/app:ro" {
		t.Errorf("bind[0] = %q", binds[0])
	}
	if binds[1] != "/host/bundles/app1/b1_lib:/blockyard-lib:ro" {
		t.Errorf("bind[1] = %q", binds[1])
	}
}

func TestMountConfig_WorkerMounts_BindMode_NoLibrary(t *testing.T) {
	mc := MountConfig{}
	binds, mounts := mc.WorkerMounts("/host/bundles/app1/b1", "", "/app")
	if len(mounts) != 0 {
		t.Fatalf("expected no mounts, got %d", len(mounts))
	}
	if len(binds) != 1 {
		t.Fatalf("expected 1 bind, got %d", len(binds))
	}
}

func TestMountConfig_WorkerMounts_VolumeMode(t *testing.T) {
	mc := MountConfig{
		VolumeName:     "blockyard-data",
		BundleBasePath: "/data/bundles",
	}

	binds, mounts := mc.WorkerMounts("/data/bundles/app1/b1", "/data/bundles/app1/b1_lib", "/app")
	if len(binds) != 0 {
		t.Fatalf("expected no binds in volume mode, got %d", len(binds))
	}
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}

	assertVolumeMount(t, mounts[0], "blockyard-data", "/app", true, "app1/b1")
	assertVolumeMount(t, mounts[1], "blockyard-data", "/blockyard-lib", true, "app1/b1_lib")
}

func TestMountConfig_BuildMounts_BindMode(t *testing.T) {
	mc := MountConfig{}

	binds, mounts := mc.BuildMounts("/host/app1/b1", "/host/app1/b1_lib", "/host/.rv-cache/rv")
	if len(mounts) != 0 {
		t.Fatalf("expected no mounts in bind mode, got %d", len(mounts))
	}
	if len(binds) != 3 {
		t.Fatalf("expected 3 binds, got %d: %v", len(binds), binds)
	}
	if binds[0] != "/host/app1/b1:/app:ro" {
		t.Errorf("bind[0] = %q", binds[0])
	}
	if binds[1] != "/host/app1/b1_lib:"+bundle.BuildContainerLibPath {
		t.Errorf("bind[1] = %q", binds[1])
	}
	if binds[2] != "/host/.rv-cache/rv:/usr/local/bin/rv:ro" {
		t.Errorf("bind[2] = %q", binds[2])
	}
}

func TestMountConfig_BuildMounts_VolumeMode(t *testing.T) {
	mc := MountConfig{
		VolumeName:     "blockyard-data",
		BundleBasePath: "/data/bundles",
	}

	binds, mounts := mc.BuildMounts(
		"/data/bundles/app1/b1",
		"/data/bundles/app1/b1_lib",
		"/data/bundles/.rv-cache/rv-v0.19.0/rv",
	)
	if len(binds) != 0 {
		t.Fatalf("expected no binds, got %d", len(binds))
	}
	if len(mounts) != 3 {
		t.Fatalf("expected 3 mounts, got %d", len(mounts))
	}

	assertVolumeMount(t, mounts[0], "blockyard-data", "/app", true, "app1/b1")
	assertVolumeMount(t, mounts[1], "blockyard-data", bundle.BuildContainerLibPath, false, "app1/b1_lib")
	assertVolumeMount(t, mounts[2], "blockyard-data", "/usr/local/bin/rv", true, ".rv-cache/rv-v0.19.0/rv")
}

func TestMountConfig_Subpath(t *testing.T) {
	mc := MountConfig{
		VolumeName:     "vol",
		BundleBasePath: "/data/bundles",
	}

	tests := []struct {
		input string
		want  string
	}{
		{"/data/bundles/app1/b1", "app1/b1"},
		{"/data/bundles/.rv-cache/rv-v0.19.0/rv", ".rv-cache/rv-v0.19.0/rv"},
		{"/data/bundles", ""},
	}
	for _, tt := range tests {
		got := mc.subpath(tt.input)
		if got != tt.want {
			t.Errorf("subpath(%q) = %q, want %q", tt.input, got, tt.want)
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
