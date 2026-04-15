package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/integration"
)

func TestWorkerKeyFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".worker-key")

	// First call: generates and writes.
	key1, err := loadOrCreateWorkerKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	// Second call: reads existing.
	key2, err := loadOrCreateWorkerKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	// Verify same key via token round-trip.
	tok1, _ := testToken(key1)
	tok2, _ := testToken(key2)
	_, err = auth.DecodeSessionToken(tok1, key2)
	if err != nil {
		t.Fatalf("token signed with key1 should verify with key2: %v", err)
	}
	_, err = auth.DecodeSessionToken(tok2, key1)
	if err != nil {
		t.Fatalf("token signed with key2 should verify with key1: %v", err)
	}
}

func TestWorkerKeyFilePermissions(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".worker-key")

	_, err := loadOrCreateWorkerKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("expected 0600 permissions, got %04o", perm)
	}
}

func TestWorkerKeyFileCorrupt(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".worker-key")

	os.WriteFile(keyPath, []byte("not-valid-base64!@#$"), 0o600)

	_, err := loadOrCreateWorkerKeyFile(keyPath)
	if err == nil {
		t.Fatal("expected error for corrupt key file")
	}
}

func TestWorkerKeyFileWrongLength(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".worker-key")

	// Write valid base64 but wrong key length (16 bytes instead of 32).
	short := make([]byte, 16)
	rand.Read(short)
	os.WriteFile(keyPath, []byte(base64.RawURLEncoding.EncodeToString(short)), 0o600)

	_, err := loadOrCreateWorkerKeyFile(keyPath)
	if err == nil {
		t.Fatal("expected error for wrong-length key")
	}
}

func TestLoadOrCreateWorkerKeyNoVault(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Storage.BundleServerPath = dir

	// No vault client — uses file path.
	key1, err := LoadOrCreateWorkerKey(context.Background(), nil, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Second call — reads from file.
	key2, err := LoadOrCreateWorkerKey(context.Background(), nil, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Verify same key via token round-trip.
	tok, _ := testToken(key1)
	_, err = auth.DecodeSessionToken(tok, key2)
	if err != nil {
		t.Fatal("keys should match across calls")
	}
}

func TestLoadOrCreateWorkerKeyVaultError(t *testing.T) {
	// Vault returns 500 for all requests — a transient error, not ErrNotFound.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client := integration.NewClient(srv.URL, integration.StaticAdmin(func() string { return "test-token" }))
	cfg := &config.Config{}
	cfg.Storage.BundleServerPath = t.TempDir()

	_, err := LoadOrCreateWorkerKey(context.Background(), client, cfg)
	if err == nil {
		t.Fatal("expected error when vault returns 500")
	}
}

// testToken creates a dummy worker token for verification.
func testToken(key *auth.SigningKey) (string, error) {
	claims := &auth.SessionTokenClaims{
		Sub: "worker:test",
		App: "test-app",
		Wid: "test-worker",
		Iat: time.Now().Unix(),
		Exp: time.Now().Add(15 * time.Minute).Unix(),
	}
	return auth.EncodeSessionToken(claims, key)
}
