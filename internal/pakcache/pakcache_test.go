package pakcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/preflight"
)

// stubBackend implements backend.Backend with a configurable Build hook.
type stubBackend struct {
	buildFn func(ctx context.Context, spec backend.BuildSpec) (backend.BuildResult, error)
}

func (b *stubBackend) Build(ctx context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
	if b.buildFn != nil {
		return b.buildFn(ctx, spec)
	}
	return backend.BuildResult{Success: true}, nil
}

func (b *stubBackend) Spawn(context.Context, backend.WorkerSpec) error             { return nil }
func (b *stubBackend) Stop(context.Context, string) error                          { return nil }
func (b *stubBackend) HealthCheck(context.Context, string) bool                    { return true }
func (b *stubBackend) Logs(context.Context, string) (backend.LogStream, error)     { return backend.LogStream{}, nil }
func (b *stubBackend) Addr(context.Context, string) (string, error)                { return "", nil }
func (b *stubBackend) ListManaged(context.Context) ([]backend.ManagedResource, error) { return nil, nil }
func (b *stubBackend) RemoveResource(context.Context, backend.ManagedResource) error { return nil }
func (b *stubBackend) WorkerResourceUsage(context.Context, string) (*backend.WorkerResourceUsageResult, error) {
	return &backend.WorkerResourceUsageResult{}, nil
}
func (b *stubBackend) UpdateResources(_ context.Context, _ string, _ int64, _ int64) error {
	return nil
}
func (b *stubBackend) CleanupOrphanResources(context.Context) error { return nil }
func (b *stubBackend) Preflight(context.Context) (*preflight.Report, error) {
	return &preflight.Report{}, nil
}

// --- EnsureInstalled tests ---

func TestEnsureInstalled_InvalidVersion(t *testing.T) {
	_, err := EnsureInstalled(context.Background(), &stubBackend{}, "img", "bogus", t.TempDir())
	if err == nil {
		t.Fatal("expected error for invalid version")
	}
	if !strings.Contains(err.Error(), "invalid pak_version") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureInstalled_CacheHit(t *testing.T) {
	cache := t.TempDir()
	os.MkdirAll(filepath.Join(cache, "pak-stable"), 0o755)

	be := &stubBackend{buildFn: func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		t.Fatal("Build must not be called on cache hit")
		return backend.BuildResult{}, nil
	}}

	got, err := EnsureInstalled(context.Background(), be, "img", "stable", cache)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(cache, "pak-stable") {
		t.Fatalf("unexpected path: %s", got)
	}
}

func TestEnsureInstalled_ChannelStale_Refreshes(t *testing.T) {
	cache := t.TempDir()
	pakDir := filepath.Join(cache, "pak-stable")
	os.MkdirAll(pakDir, 0o755)

	// Push modtime past the 24-hour TTL.
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(pakDir, old, old); err != nil {
		t.Fatal(err)
	}

	var buildCalled bool
	be := &stubBackend{buildFn: func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		buildCalled = true
		return backend.BuildResult{Success: true}, nil
	}}

	got, err := EnsureInstalled(context.Background(), be, "img", "stable", cache)
	if err != nil {
		t.Fatal(err)
	}
	if !buildCalled {
		t.Fatal("expected Build to be called for stale channel cache")
	}
	if got != pakDir {
		t.Fatalf("unexpected path: %s", got)
	}
}

func TestEnsureInstalled_DevelChannelExpires(t *testing.T) {
	cache := t.TempDir()
	pakDir := filepath.Join(cache, "pak-devel")
	os.MkdirAll(pakDir, 0o755)

	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(pakDir, old, old); err != nil {
		t.Fatal(err)
	}

	var buildCalled bool
	be := &stubBackend{buildFn: func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		buildCalled = true
		return backend.BuildResult{Success: true}, nil
	}}

	_, err := EnsureInstalled(context.Background(), be, "img", "devel", cache)
	if err != nil {
		t.Fatal(err)
	}
	if !buildCalled {
		t.Fatal("devel is a channel; stale cache should trigger re-install")
	}
}

func TestEnsureInstalled_PinnedNeverExpires(t *testing.T) {
	cache := t.TempDir()
	pakDir := filepath.Join(cache, "pak-pinned")
	os.MkdirAll(pakDir, 0o755)

	// 100 hours old — pinned must never expire.
	old := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(pakDir, old, old); err != nil {
		t.Fatal(err)
	}

	be := &stubBackend{buildFn: func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		t.Fatal("Build must not be called for non-expiring pinned cache")
		return backend.BuildResult{}, nil
	}}

	got, err := EnsureInstalled(context.Background(), be, "img", "pinned", cache)
	if err != nil {
		t.Fatal(err)
	}
	if got != pakDir {
		t.Fatalf("unexpected path: %s", got)
	}
}

func TestEnsureInstalled_PinnedUsesStableChannel(t *testing.T) {
	cache := t.TempDir()
	var rCmd string
	be := &stubBackend{buildFn: func(_ context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
		if len(spec.Cmd) >= 4 {
			rCmd = spec.Cmd[3]
		}
		return backend.BuildResult{Success: true}, nil
	}}

	_, err := EnsureInstalled(context.Background(), be, "img", "pinned", cache)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rCmd, "/stable/") {
		t.Fatalf("pinned should download from stable channel, got: %s", rCmd)
	}
}

func TestEnsureInstalled_FreshInstall(t *testing.T) {
	cache := t.TempDir()
	var buildCalled bool
	be := &stubBackend{buildFn: func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		buildCalled = true
		return backend.BuildResult{Success: true}, nil
	}}

	got, err := EnsureInstalled(context.Background(), be, "img", "stable", cache)
	if err != nil {
		t.Fatal(err)
	}
	if !buildCalled {
		t.Fatal("expected Build for fresh install")
	}
	if got != filepath.Join(cache, "pak-stable") {
		t.Fatalf("unexpected path: %s", got)
	}
	// The directory should exist after install.
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Fatalf("expected directory at %s", got)
	}
}

func TestEnsureInstalled_BuildError(t *testing.T) {
	cache := t.TempDir()
	be := &stubBackend{buildFn: func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		return backend.BuildResult{}, fmt.Errorf("docker daemon unavailable")
	}}

	_, err := EnsureInstalled(context.Background(), be, "img", "stable", cache)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "install pak") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureInstalled_BuildFailure(t *testing.T) {
	cache := t.TempDir()
	be := &stubBackend{buildFn: func(context.Context, backend.BuildSpec) (backend.BuildResult, error) {
		return backend.BuildResult{
			Success:  false,
			ExitCode: 1,
			Logs:     "line1\nline2\nError: package not found\n",
		}, nil
	}}

	_, err := EnsureInstalled(context.Background(), be, "img", "stable", cache)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "install pak failed (exit 1)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureInstalled_VerifyBuildSpec(t *testing.T) {
	cache := t.TempDir()
	var spec backend.BuildSpec
	be := &stubBackend{buildFn: func(_ context.Context, s backend.BuildSpec) (backend.BuildResult, error) {
		spec = s
		return backend.BuildResult{Success: true}, nil
	}}

	_, err := EnsureInstalled(context.Background(), be, "r-base:4.4", "rc", cache)
	if err != nil {
		t.Fatal(err)
	}

	if spec.Image != "r-base:4.4" {
		t.Fatalf("image = %q, want r-base:4.4", spec.Image)
	}
	if spec.AppID != "_system" {
		t.Fatalf("AppID = %q, want _system", spec.AppID)
	}
	if !strings.HasPrefix(spec.BundleID, "pak-install-") {
		t.Fatalf("BundleID = %q, want pak-install-* prefix", spec.BundleID)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Target != "/pak-output" {
		t.Fatalf("expected single mount at /pak-output, got %+v", spec.Mounts)
	}
	if spec.Mounts[0].ReadOnly {
		t.Fatal("output mount must be writable")
	}
	if len(spec.Cmd) < 4 || !strings.Contains(spec.Cmd[3], "/rc/") {
		t.Fatalf("expected R command with /rc/ channel, got %v", spec.Cmd)
	}
	if spec.Labels["dev.blockyard/role"] != "build" {
		t.Fatalf("missing build label")
	}
}

// --- lastLines / splitLines ---

func TestLastLines(t *testing.T) {
	tests := []struct {
		name, input, want string
		n                 int
	}{
		{"empty", "", "", 5},
		{"fewer_than_n", "a\nb", "a\nb", 5},
		{"exact_n", "a\nb\nc", "a\nb\nc", 3},
		{"more_than_n", "a\nb\nc\nd\ne", "d\ne\n", 2},
		{"single_line", "hello", "hello", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lastLines(tt.input, tt.n)
			if got != tt.want {
				t.Fatalf("lastLines(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
			}
		})
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty", "", nil},
		{"single", "hello", []string{"hello"}},
		{"multi", "a\nb\nc", []string{"a", "b", "c"}},
		{"trailing_newline", "a\nb\n", []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitLines(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("splitLines(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("splitLines(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}
