package mount

import (
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
)

var testSources = []config.DataMountSource{
	{Name: "models", Path: "/data/shared-models"},
	{Name: "scratch", Path: "/data/scratch"},
}

func TestValidate_ValidMounts(t *testing.T) {
	mounts := []db.DataMountRow{
		{Source: "models", Target: "/data/models", ReadOnly: true},
	}
	if err := Validate(mounts, testSources); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_ValidSubpath(t *testing.T) {
	mounts := []db.DataMountRow{
		{Source: "models/v2", Target: "/data/models", ReadOnly: true},
	}
	if err := Validate(mounts, testSources); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_UnknownSource(t *testing.T) {
	mounts := []db.DataMountRow{
		{Source: "unknown", Target: "/data/x", ReadOnly: true},
	}
	err := Validate(mounts, testSources)
	if err == nil || !strings.Contains(err.Error(), "unknown source") {
		t.Errorf("expected unknown source error, got: %v", err)
	}
}

func TestValidate_SourceSubpathTraversal(t *testing.T) {
	mounts := []db.DataMountRow{
		{Source: "models/../secret", Target: "/data/x", ReadOnly: true},
	}
	err := Validate(mounts, testSources)
	if err == nil || !strings.Contains(err.Error(), "..") {
		t.Errorf("expected path traversal error, got: %v", err)
	}
}

func TestValidate_TargetNotAbsolute(t *testing.T) {
	mounts := []db.DataMountRow{
		{Source: "models", Target: "relative/path", ReadOnly: true},
	}
	err := Validate(mounts, testSources)
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Errorf("expected absolute path error, got: %v", err)
	}
}

func TestValidate_TargetTraversal(t *testing.T) {
	mounts := []db.DataMountRow{
		{Source: "models", Target: "/data/../etc/passwd", ReadOnly: true},
	}
	err := Validate(mounts, testSources)
	if err == nil || !strings.Contains(err.Error(), "..") {
		t.Errorf("expected path traversal error, got: %v", err)
	}
}

func TestValidate_ReservedTarget(t *testing.T) {
	reserved := []string{"/app", "/app/data", "/tmp", "/var/run/blockyard",
		"/blockyard-lib", "/blockyard-lib-store", "/transfer"}
	for _, target := range reserved {
		mounts := []db.DataMountRow{
			{Source: "models", Target: target, ReadOnly: true},
		}
		err := Validate(mounts, testSources)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("target %q: expected reserved path error, got: %v", target, err)
		}
	}
}

func TestValidate_NonReservedTarget(t *testing.T) {
	// /application is not /app, so it should pass.
	mounts := []db.DataMountRow{
		{Source: "models", Target: "/application", ReadOnly: true},
	}
	if err := Validate(mounts, testSources); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_DuplicateTargets(t *testing.T) {
	mounts := []db.DataMountRow{
		{Source: "models", Target: "/data/models", ReadOnly: true},
		{Source: "scratch", Target: "/data/models", ReadOnly: true},
	}
	err := Validate(mounts, testSources)
	if err == nil || !strings.Contains(err.Error(), "duplicate target") {
		t.Errorf("expected duplicate target error, got: %v", err)
	}
}

func TestResolve_Basic(t *testing.T) {
	mounts := []db.DataMountRow{
		{Source: "models", Target: "/data/models", ReadOnly: true},
	}
	entries, err := Resolve(mounts, testSources)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	want := backend.MountEntry{
		Source: "/data/shared-models", Target: "/data/models", ReadOnly: true,
	}
	if entries[0] != want {
		t.Errorf("got %+v, want %+v", entries[0], want)
	}
}

func TestResolve_SubpathJoin(t *testing.T) {
	mounts := []db.DataMountRow{
		{Source: "models/v2", Target: "/data/models", ReadOnly: true},
	}
	entries, err := Resolve(mounts, testSources)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries[0].Source != "/data/shared-models/v2" {
		t.Errorf("source = %q, want %q", entries[0].Source, "/data/shared-models/v2")
	}
}

func TestResolve_MissingSource(t *testing.T) {
	mounts := []db.DataMountRow{
		{Source: "removed", Target: "/data/x", ReadOnly: true},
	}
	_, err := Resolve(mounts, testSources)
	if err == nil || !strings.Contains(err.Error(), "not found in config") {
		t.Errorf("expected not found error, got: %v", err)
	}
}

func TestResolve_ReadWrite(t *testing.T) {
	mounts := []db.DataMountRow{
		{Source: "scratch", Target: "/data/scratch", ReadOnly: false},
	}
	entries, err := Resolve(mounts, testSources)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries[0].ReadOnly {
		t.Error("expected ReadOnly=false")
	}
}
