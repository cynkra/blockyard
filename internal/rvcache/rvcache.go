package rvcache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// latestMaxAge controls how long a cached "latest" binary is considered fresh.
const latestMaxAge = 1 * time.Hour

// mu serialises downloads so concurrent restores don't race.
var mu sync.Mutex

// EnsureBinary returns the path to a cached rv binary for the given version.
// If the binary is not cached (or stale for "latest"), it is downloaded from
// the a2-ai/rv GitHub releases.
func EnsureBinary(ctx context.Context, cacheDir, version string) (string, error) {
	mu.Lock()
	defer mu.Unlock()

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("rvcache: create cache dir: %w", err)
	}

	dest := filepath.Join(cacheDir, "rv-"+version)

	if version == "latest" {
		if info, err := os.Stat(dest); err == nil {
			if time.Since(info.ModTime()) < latestMaxAge {
				return dest, nil
			}
		}
	} else if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	url := downloadURL(version)
	if err := download(ctx, url, dest); err != nil {
		return "", fmt.Errorf("rvcache: download rv %s: %w", version, err)
	}

	return dest, nil
}

func downloadURL(version string) string {
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64"
	}
	asset := fmt.Sprintf("rv-%s-unknown-linux-gnu", arch)

	if version == "latest" {
		return fmt.Sprintf("https://github.com/a2-ai/rv/releases/latest/download/%s", asset)
	}
	return fmt.Sprintf("https://github.com/a2-ai/rv/releases/download/%s/%s", version, asset)
}

func download(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".rv-download-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return err
	}

	return nil
}
