package server

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
)

const workerTokenTTL = 15 * time.Minute

// workerToken creates a signed HMAC token for a worker.
func workerToken(signingKey *auth.SigningKey, appID, workerID string) (string, error) {
	now := time.Now()
	claims := &auth.SessionTokenClaims{
		Sub: "worker:" + workerID,
		App: appID,
		Wid: workerID,
		Iat: now.Unix(),
		Exp: now.Add(workerTokenTTL).Unix(),
	}
	return auth.EncodeSessionToken(claims, signingKey)
}

// tokenDir returns the host-side directory for a worker's token file.
func tokenDir(bundleServerPath, workerID string) string {
	return filepath.Join(bundleServerPath, ".worker-tokens", workerID)
}

// writeTokenFile writes a fresh token to the worker's token file
// using atomic write (temp + rename) to prevent partial reads.
func writeTokenFile(dir string, signingKey *auth.SigningKey, appID, workerID string) error {
	token, err := workerToken(signingKey, appID, workerID)
	if err != nil {
		return err
	}
	tokenPath := filepath.Join(dir, "token")
	tmp := tokenPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, tokenPath)
}

// SpawnTokenRefresher starts a goroutine that refreshes the worker's
// token file every TTL/2. Returns the token directory and a cancel
// function. The goroutine writes an initial token synchronously before
// returning, so the token file is ready before the container starts.
func SpawnTokenRefresher(
	ctx context.Context,
	bundleServerPath string,
	signingKey *auth.SigningKey,
	appID, workerID string,
) (tokDir string, cancel func(), err error) {
	dir := tokenDir(bundleServerPath, workerID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}

	// Write initial token synchronously — container needs it at start.
	if err := writeTokenFile(dir, signingKey, appID, workerID); err != nil {
		return "", nil, err
	}

	ctx, cancel = context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(workerTokenTTL / 2) // refresh at 7.5min
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := writeTokenFile(dir, signingKey, appID, workerID); err != nil {
					slog.Warn("failed to refresh worker token",
						"worker_id", workerID, "error", err)
				}
			}
		}
	}()

	return dir, cancel, nil
}

// CleanupTokenDir removes the token directory for a worker.
func CleanupTokenDir(bundleServerPath, workerID string) {
	_ = os.RemoveAll(tokenDir(bundleServerPath, workerID))
}
