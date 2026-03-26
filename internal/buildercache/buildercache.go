package buildercache

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

var mu sync.Mutex

// preInstalledPath is the well-known location where the Docker image
// places the pre-built by-builder binary.
const preInstalledPath = "/usr/local/lib/blockyard/by-builder"

// EnsureCached returns the path to the by-builder binary for the
// current platform. The binary must reside on a shared volume so it
// can be bind-mounted into build containers. Checks cache first, then
// copies from a pre-installed path (Docker image), then falls back to
// compiling from source (development).
func EnsureCached(cachePath, version string) (string, error) {
	mu.Lock()
	defer mu.Unlock()

	name := fmt.Sprintf("by-builder-%s-linux-%s", version, runtime.GOARCH)
	binPath := filepath.Join(cachePath, name)
	if fileExists(binPath) {
		return binPath, nil
	}

	if err := os.MkdirAll(cachePath, 0o755); err != nil {
		return "", fmt.Errorf("create builder cache dir: %w", err)
	}

	// Try pre-installed binary (production Docker image).
	if fileExists(preInstalledPath) {
		slog.Info("copying pre-installed by-builder to cache",
			"src", preInstalledPath, "dst", binPath)
		if err := copyBinary(preInstalledPath, binPath); err != nil {
			return "", fmt.Errorf("copy pre-installed by-builder: %w", err)
		}
		return binPath, nil
	}

	// Fall back to compiling from source (development).
	slog.Info("compiling by-builder", "version", version, "arch", runtime.GOARCH)
	if err := buildFromSource(binPath); err != nil {
		return "", fmt.Errorf("compile by-builder: %w", err)
	}

	slog.Info("by-builder cached", "path", binPath)
	return binPath, nil
}

// copyBinary copies a file preserving its permissions.
func copyBinary(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// buildFromSource compiles the by-builder binary from the Go module.
// Produces a static binary (CGO_ENABLED=0) suitable for any Linux
// container.
func buildFromSource(binPath string) error {
	// Find the module root by looking for go.mod relative to this
	// package's source location.
	modRoot, err := findModuleRoot()
	if err != nil {
		return err
	}

	tmpPath := binPath + ".tmp"
	cmd := exec.Command("go", "build", "-o", tmpPath, "./cmd/by-builder/")
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH="+runtime.GOARCH,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", string(out), err)
	}
	return os.Rename(tmpPath, binPath)
}

// findModuleRoot walks up from the current working directory to find
// go.mod, which identifies the module root.
func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
