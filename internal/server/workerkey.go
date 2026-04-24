package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/integration"
)

// LoadOrCreateWorkerKey resolves the worker signing key. It tries
// three sources in order:
//
//  1. The vault (if configured) -- read or generate + store
//  2. File ({bundle_server_path}/.worker-key) -- read existing
//  3. Generate new + write to file
//
// This ensures both the old and new server use the same key during
// a rolling update. When the vault is not available, the file path
// provides persistence across restarts.
func LoadOrCreateWorkerKey(
	ctx context.Context,
	vaultClient *integration.Client,
	cfg *config.Config,
) (*auth.SigningKey, error) {
	// Path 1: vault.
	if vaultClient != nil {
		key, err := integration.ResolveWorkerKey(ctx, vaultClient)
		if err != nil {
			return nil, fmt.Errorf("resolve worker key via vault: %w", err)
		}
		return auth.NewSigningKey(key), nil
	}

	// Path 2/3: file-based.
	keyPath := filepath.Join(cfg.Storage.BundleServerPath, ".worker-key")
	return loadOrCreateWorkerKeyFile(keyPath)
}

// loadOrCreateWorkerKeyFile reads the key from disk if it exists,
// or generates a new one and writes it. File permissions: 0600.
func loadOrCreateWorkerKeyFile(path string) (*auth.SigningKey, error) {
	path = filepath.Clean(path)

	// Try reading existing file.
	data, err := os.ReadFile(path)
	if err == nil {
		key, err := base64.RawURLEncoding.DecodeString(string(data))
		if err != nil {
			return nil, fmt.Errorf("decode worker key file %s: %w", path, err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("worker key file %s has wrong length: %d", path, len(key))
		}
		slog.Info("worker signing key loaded from file", "path", path)
		return auth.NewSigningKey(key), nil
	}

	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read worker key file %s: %w", path, err)
	}

	// Generate new key and write to file.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate worker signing key: %w", err)
	}

	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create worker key directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, fmt.Errorf("write worker key file: %w", err)
	}

	slog.Info("auto-generated worker signing key (stored to file)", "path", path)
	return auth.NewSigningKey(raw), nil
}
