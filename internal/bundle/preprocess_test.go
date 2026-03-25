package bundle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/backend"
)

// ppBackend implements backend.Backend with a configurable Build hook.
type ppBackend struct {
	buildFn func(ctx context.Context, spec backend.BuildSpec) (backend.BuildResult, error)
}

func (b *ppBackend) Build(ctx context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
	if b.buildFn != nil {
		return b.buildFn(ctx, spec)
	}
	return backend.BuildResult{Success: true}, nil
}

func (b *ppBackend) Spawn(context.Context, backend.WorkerSpec) error                  { return nil }
func (b *ppBackend) Stop(context.Context, string) error                               { return nil }
func (b *ppBackend) HealthCheck(context.Context, string) bool                         { return true }
func (b *ppBackend) Logs(context.Context, string) (backend.LogStream, error)          { return backend.LogStream{}, nil }
func (b *ppBackend) Addr(context.Context, string) (string, error)                     { return "", nil }
func (b *ppBackend) ListManaged(context.Context) ([]backend.ManagedResource, error)   { return nil, nil }
func (b *ppBackend) RemoveResource(context.Context, backend.ManagedResource) error    { return nil }

// --- preProcess tests ---

func TestPreProcess_Success(t *testing.T) {
	tmp := t.TempDir()
	unpacked := filepath.Join(tmp, "app", "b-1")
	os.MkdirAll(unpacked, 0o755)
	os.WriteFile(filepath.Join(unpacked, "app.R"), []byte("library(shiny)"), 0o644)

	pakDir := filepath.Join(tmp, "pak")
	os.MkdirAll(pakDir, 0o755)

	be := &ppBackend{buildFn: func(_ context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
		// Simulate the container writing a DESCRIPTION to the output mount.
		for _, m := range spec.Mounts {
			if m.Target == "/output" && !m.ReadOnly {
				if err := os.WriteFile(filepath.Join(m.Source, "DESCRIPTION"),
					[]byte("Package: app\nVersion: 0.0.1\nImports: shiny\n"), 0o644); err != nil {
					return backend.BuildResult{}, err
				}
			}
		}
		return backend.BuildResult{Success: true}, nil
	}}

	p := RestoreParams{
		AppID:    "test-app",
		BundleID: "b-1",
		Image:    "r-base:4.4",
		BasePath: tmp,
		Paths:    Paths{Unpacked: unpacked},
	}

	if err := preProcess(context.Background(), be, pakDir, p); err != nil {
		t.Fatal(err)
	}

	desc, err := os.ReadFile(filepath.Join(unpacked, "DESCRIPTION"))
	if err != nil {
		t.Fatal("DESCRIPTION not written to unpacked dir:", err)
	}
	if !strings.Contains(string(desc), "Imports: shiny") {
		t.Fatalf("unexpected DESCRIPTION: %s", desc)
	}
}

func TestPreProcess_BuildError(t *testing.T) {
	tmp := t.TempDir()
	unpacked := filepath.Join(tmp, "app", "b-1")
	os.MkdirAll(unpacked, 0o755)

	be := &ppBackend{buildFn: func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		return backend.BuildResult{}, fmt.Errorf("container runtime error")
	}}

	p := RestoreParams{
		AppID: "a", BundleID: "b", BasePath: tmp,
		Paths: Paths{Unpacked: unpacked},
	}

	err := preProcess(context.Background(), be, "/pak", p)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "preprocess") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPreProcess_BuildFailure(t *testing.T) {
	tmp := t.TempDir()
	unpacked := filepath.Join(tmp, "app", "b-1")
	os.MkdirAll(unpacked, 0o755)

	be := &ppBackend{buildFn: func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		return backend.BuildResult{
			Success:  false,
			ExitCode: 1,
			Logs:     "Error in library(pak): no such package\n",
		}, nil
	}}

	p := RestoreParams{
		AppID: "a", BundleID: "b", BasePath: tmp,
		Paths: Paths{Unpacked: unpacked},
	}

	err := preProcess(context.Background(), be, "/pak", p)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "script scanning failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPreProcess_NoDESCRIPTION(t *testing.T) {
	tmp := t.TempDir()
	unpacked := filepath.Join(tmp, "app", "b-1")
	os.MkdirAll(unpacked, 0o755)

	// Build succeeds but doesn't write a DESCRIPTION file.
	be := &ppBackend{buildFn: func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		return backend.BuildResult{Success: true}, nil
	}}

	p := RestoreParams{
		AppID: "a", BundleID: "b", BasePath: tmp,
		Paths: Paths{Unpacked: unpacked},
	}

	err := preProcess(context.Background(), be, "/pak", p)
	if err == nil {
		t.Fatal("expected error when DESCRIPTION not produced")
	}
	if !strings.Contains(err.Error(), "copy DESCRIPTION") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPreProcess_VerifyBuildSpec(t *testing.T) {
	tmp := t.TempDir()
	unpacked := filepath.Join(tmp, "app", "b-1")
	os.MkdirAll(unpacked, 0o755)

	var spec backend.BuildSpec
	be := &ppBackend{buildFn: func(_ context.Context, s backend.BuildSpec) (backend.BuildResult, error) {
		spec = s
		// Write DESCRIPTION so the test doesn't fail on copy.
		for _, m := range s.Mounts {
			if m.Target == "/output" {
				os.WriteFile(filepath.Join(m.Source, "DESCRIPTION"), []byte("Package: app\n"), 0o644)
			}
		}
		return backend.BuildResult{Success: true}, nil
	}}

	p := RestoreParams{
		AppID: "myapp", BundleID: "b-42", Image: "r-base:4.4", BasePath: tmp,
		Paths: Paths{Unpacked: unpacked},
	}

	if err := preProcess(context.Background(), be, "/pak-lib", p); err != nil {
		t.Fatal(err)
	}

	if spec.BundleID != "b-42-preprocess" {
		t.Fatalf("BundleID = %q, want b-42-preprocess", spec.BundleID)
	}
	if spec.Image != "r-base:4.4" {
		t.Fatalf("Image = %q, want r-base:4.4", spec.Image)
	}

	// Expect 3 mounts: /app (ro), /pak (ro), /output (rw).
	if len(spec.Mounts) != 3 {
		t.Fatalf("expected 3 mounts, got %d", len(spec.Mounts))
	}
	mountTargets := map[string]bool{}
	for _, m := range spec.Mounts {
		mountTargets[m.Target] = true
		switch m.Target {
		case "/app":
			if !m.ReadOnly {
				t.Fatal("/app mount must be read-only")
			}
			if m.Source != unpacked {
				t.Fatalf("/app source = %q, want %q", m.Source, unpacked)
			}
		case "/pak":
			if !m.ReadOnly {
				t.Fatal("/pak mount must be read-only")
			}
			if m.Source != "/pak-lib" {
				t.Fatalf("/pak source = %q, want /pak-lib", m.Source)
			}
		case "/output":
			if m.ReadOnly {
				t.Fatal("/output mount must be writable")
			}
		}
	}
	for _, target := range []string{"/app", "/pak", "/output"} {
		if !mountTargets[target] {
			t.Fatalf("missing mount target %s", target)
		}
	}
}

// --- copyFile ---

func TestCopyFile(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src.txt")
	dst := filepath.Join(tmp, "dst.txt")

	content := "Package: app\nImports: shiny\n"
	os.WriteFile(src, []byte(content), 0o644)

	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("copy mismatch: got %q", got)
	}
}

func TestCopyFile_SourceMissing(t *testing.T) {
	tmp := t.TempDir()
	err := copyFile(filepath.Join(tmp, "nope"), filepath.Join(tmp, "dst"))
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}
