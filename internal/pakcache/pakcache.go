package pakcache

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
)

// mu serialises pak installations so concurrent builds don't race.
var mu sync.Mutex

// EnsureInstalled downloads the pak R package to the cache directory
// if not already present. Returns the path to the cached pak package
// directory (suitable for mounting into build containers).
//
// The cached pak is a fully installed R package tree — the build
// container adds it to .libPaths() and calls pak functions directly.
func EnsureInstalled(ctx context.Context, be backend.Backend,
	image, version, cachePath string) (string, error) {

	mu.Lock()
	defer mu.Unlock()

	pakDir := filepath.Join(cachePath, "pak-"+version)
	if dirExists(pakDir) {
		return pakDir, nil
	}

	if err := os.MkdirAll(cachePath, 0o755); err != nil {
		return "", fmt.Errorf("create pak cache dir: %w", err)
	}

	// Install pak into a temp directory using a short-lived container.
	installCmd := fmt.Sprintf(
		`install.packages("pak", lib="/pak-output", repos=sprintf(`+
			`"https://r-lib.github.io/p/pak/%s/%%s/%%s/%%s", `+
			`.Platform$pkgType, R.Version()$os, R.Version()$arch))`,
		version)

	tmpDir, err := os.MkdirTemp(cachePath, ".pak-install-")
	if err != nil {
		return "", fmt.Errorf("create pak temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck // best-effort cleanup

	spec := backend.BuildSpec{
		AppID:    "_system",
		BundleID: "pak-install-" + uuid.New().String()[:8],
		Image:    image,
		Cmd:      []string{"R", "--vanilla", "-e", installCmd},
		Mounts: []backend.MountEntry{
			{Source: tmpDir, Target: "/pak-output", ReadOnly: false},
		},
		Labels: map[string]string{
			"dev.blockyard/managed": "true",
			"dev.blockyard/role":    "build",
		},
	}

	slog.Info("installing pak into cache", "version", version)
	result, err := be.Build(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("install pak: %w", err)
	}
	if !result.Success {
		return "", fmt.Errorf("install pak failed (exit %d): %s",
			result.ExitCode, lastLines(result.Logs, 10))
	}

	if err := os.Rename(tmpDir, pakDir); err != nil {
		return "", fmt.Errorf("move pak cache: %w", err)
	}

	slog.Info("pak cached", "version", version, "path", pakDir)
	return pakDir, nil
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func lastLines(s string, n int) string {
	lines := splitLines(s)
	if len(lines) <= n {
		return s
	}
	result := ""
	for _, l := range lines[len(lines)-n:] {
		result += l + "\n"
	}
	return result
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
