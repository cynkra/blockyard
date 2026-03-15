package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadTokenFile reads a vault token from the given file path.
// Returns an empty string and nil error if the file does not exist.
func ReadTokenFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// WriteTokenFile atomically writes a vault token to the given file
// path. Uses temp file + rename for crash safety. File mode is 0600.
func WriteTokenFile(path, token string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".vault-token-*")
	if err != nil {
		return fmt.Errorf("write token file: create temp: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.WriteString(token); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write token file: write: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write token file: chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("write token file: close: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("write token file: rename: %w", err)
	}
	return nil
}
