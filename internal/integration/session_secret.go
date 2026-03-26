package integration

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
)

// ResolveSessionSecret reads or generates session_secret from vault.
// If the key exists at secret/data/blockyard/server-secrets, it's used.
// Otherwise, a new 32-byte random value is generated, stored, and returned.
func ResolveSessionSecret(ctx context.Context, client *Client) (string, error) {
	const kvPath = "blockyard/server-secrets"

	// Try reading existing.
	data, err := client.KVRead(ctx, kvPath, client.AdminToken())
	if err == nil {
		if v, ok := data["session_secret"]; ok {
			if s, ok := v.(string); ok && s != "" {
				slog.Info("session_secret loaded from vault")
				return s, nil
			}
		}
	}

	// Generate new.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session_secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)

	// Store in vault.
	if err := client.KVWrite(ctx, kvPath, map[string]any{
		"session_secret": secret,
	}); err != nil {
		return "", fmt.Errorf("store session_secret in vault: %w", err)
	}

	slog.Info("auto-generated session_secret (stored in vault)")
	return secret, nil
}
