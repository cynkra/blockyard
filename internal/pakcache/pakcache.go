package pakcache

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/backend"
)

// channelMaxAge is how long a channel-based cache entry (e.g. "stable")
// is considered fresh before re-downloading.
const channelMaxAge = 24 * time.Hour

// validVersions lists the accepted pak_version values. "pinned" installs
// from "stable" once and never refreshes — an escape hatch for operators
// who want to lock to a specific pak version without expiry.
var validVersions = map[string]bool{
	"stable": true, "rc": true, "devel": true, "pinned": true,
}

// channels are the rolling release channels whose cache can go stale.
var channels = map[string]bool{"stable": true, "rc": true, "devel": true}

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

	if !validVersions[version] {
		return "", fmt.Errorf("invalid pak_version %q (must be stable, rc, devel, or pinned)", version)
	}

	mu.Lock()
	defer mu.Unlock()

	pakDir := filepath.Join(cachePath, "pak-"+version)
	if info, err := os.Stat(pakDir); err == nil && info.IsDir() {
		if !channels[version] || time.Since(info.ModTime()) < channelMaxAge {
			return pakDir, nil
		}
		// Channel cache is stale — remove and re-install.
		slog.Info("pak channel cache expired, refreshing",
			"version", version, "age", time.Since(info.ModTime()).Round(time.Second))
		if err := os.RemoveAll(pakDir); err != nil {
			return "", fmt.Errorf("remove stale pak cache: %w", err)
		}
	}

	if err := os.MkdirAll(cachePath, 0o755); err != nil { //nolint:gosec // G301: package cache dir, not secrets
		return "", fmt.Errorf("create pak cache dir: %w", err)
	}

	// "pinned" installs from the stable channel but never expires.
	downloadChannel := version
	if version == "pinned" {
		downloadChannel = "stable"
	}

	// Install pak into a temp directory using a short-lived container.
	installCmd := fmt.Sprintf(
		`install.packages("pak", lib="/pak-output", repos=sprintf(`+
			`"https://r-lib.github.io/p/pak/%s/%%s/%%s/%%s", `+
			`.Platform$pkgType, R.Version()$os, R.Version()$arch))`,
		downloadChannel)

	tmpDir, err := os.MkdirTemp(cachePath, ".pak-install-")
	if err != nil {
		return "", fmt.Errorf("create pak temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck // best-effort cleanup

	spec := backend.BuildSpec{
		AppID:    "_system",
		BundleID: "pak-install-" + uuid.New().String()[:8],
		Image:    image,
		Cmd:      []string{"Rscript", "--vanilla", "-e", installCmd},
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
