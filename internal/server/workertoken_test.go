package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
)

func testSigningKey(t *testing.T) *auth.SigningKey {
	t.Helper()
	return auth.NewSigningKey([]byte("test-worker-token-key-32-bytes!!"))
}

func TestWorkerTokenGenerate(t *testing.T) {
	key := testSigningKey(t)
	token, err := workerToken(key, "app-1", "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	// Verify the token is valid.
	claims, err := auth.DecodeSessionToken(token, key)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Sub != "worker:worker-1" {
		t.Errorf("Sub = %q, want %q", claims.Sub, "worker:worker-1")
	}
	if claims.App != "app-1" {
		t.Errorf("App = %q, want %q", claims.App, "app-1")
	}
	if claims.Wid != "worker-1" {
		t.Errorf("Wid = %q, want %q", claims.Wid, "worker-1")
	}
	// Token should expire in ~15 minutes.
	ttl := time.Unix(claims.Exp, 0).Sub(time.Unix(claims.Iat, 0))
	if ttl != workerTokenTTL {
		t.Errorf("TTL = %v, want %v", ttl, workerTokenTTL)
	}
}

func TestWorkerTokenExpired(t *testing.T) {
	key := testSigningKey(t)
	// Create a token with expiry in the past.
	claims := &auth.SessionTokenClaims{
		Sub: "worker:w1",
		App: "app-1",
		Wid: "w1",
		Iat: time.Now().Add(-20 * time.Minute).Unix(),
		Exp: time.Now().Add(-5 * time.Minute).Unix(),
	}
	token, err := auth.EncodeSessionToken(claims, key)
	if err != nil {
		t.Fatal(err)
	}

	_, err = auth.DecodeSessionToken(token, key)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestWorkerTokenWrongKey(t *testing.T) {
	key1 := testSigningKey(t)
	key2 := auth.NewSigningKey([]byte("different-key-32-bytes-long!!!!!"))

	token, err := workerToken(key1, "app-1", "worker-1")
	if err != nil {
		t.Fatal(err)
	}

	_, err = auth.DecodeSessionToken(token, key2)
	if err == nil {
		t.Fatal("expected error for wrong key")
	}
}

func TestWriteTokenFile(t *testing.T) {
	dir := t.TempDir()
	key := testSigningKey(t)

	err := writeTokenFile(dir, key, "app-1", "worker-1")
	if err != nil {
		t.Fatal(err)
	}

	// Token file should exist and be non-empty.
	data, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty token file")
	}

	// Token should be valid.
	claims, err := auth.DecodeSessionToken(string(data), key)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Wid != "worker-1" {
		t.Errorf("Wid = %q, want %q", claims.Wid, "worker-1")
	}
}

func TestSpawnTokenRefresher(t *testing.T) {
	baseDir := t.TempDir()
	key := testSigningKey(t)

	tokDir, cancel, err := SpawnTokenRefresher(
		context.Background(), baseDir, key, "app-1", "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	// Token directory should exist.
	if _, err := os.Stat(tokDir); err != nil {
		t.Fatalf("token dir should exist: %v", err)
	}

	// Initial token should be written synchronously.
	data, err := os.ReadFile(filepath.Join(tokDir, "token"))
	if err != nil {
		t.Fatal("initial token should be written:", err)
	}
	if len(data) == 0 {
		t.Fatal("initial token should be non-empty")
	}
}

func TestTokenRefresherCancelled(t *testing.T) {
	baseDir := t.TempDir()
	key := testSigningKey(t)

	tokDir, cancel, err := SpawnTokenRefresher(
		context.Background(), baseDir, key, "app-1", "worker-1")
	if err != nil {
		t.Fatal(err)
	}

	// Cancel should stop the refresher goroutine.
	cancel()

	// Token file should still exist (not cleaned up by cancel).
	if _, err := os.Stat(filepath.Join(tokDir, "token")); err != nil {
		t.Fatal("token should still exist after cancel:", err)
	}
}
