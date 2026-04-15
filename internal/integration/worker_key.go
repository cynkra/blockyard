package integration

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
)

const workerKeyKVPath = "blockyard/worker-signing-key"

// ResolveWorkerKey reads or generates the worker signing key from vault.
// Follows the same pattern as ResolveSessionSecret: read if exists,
// generate + store if not. Transient vault errors are fatal --
// only ErrNotFound triggers generation.
func ResolveWorkerKey(ctx context.Context, client *Client) ([]byte, error) {
	data, err := client.KVReadAdmin(ctx, workerKeyKVPath)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("read worker key from vault: %w", err)
	}
	if err == nil {
		if v, ok := data["worker_signing_key"]; ok {
			if s, ok := v.(string); ok && s != "" {
				key, err := base64.RawURLEncoding.DecodeString(s)
				if err != nil {
					return nil, fmt.Errorf("decode worker key from vault: %w", err)
				}
				if len(key) != 32 {
					return nil, fmt.Errorf("worker key in vault has wrong length: %d", len(key))
				}
				slog.Info("worker signing key loaded from vault")
				return key, nil
			}
		}
	}

	// ErrNotFound: generate new key.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate worker signing key: %w", err)
	}

	// Store in vault.
	encoded := base64.RawURLEncoding.EncodeToString(key)
	if err := client.KVWrite(ctx, workerKeyKVPath, map[string]any{
		"worker_signing_key": encoded,
	}); err != nil {
		return nil, fmt.Errorf("store worker signing key in vault: %w", err)
	}

	slog.Info("auto-generated worker signing key (stored in vault)")
	return key, nil
}
