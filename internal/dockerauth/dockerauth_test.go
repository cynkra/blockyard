package dockerauth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/registry"
)

func writeConfig(t *testing.T, dir string, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func decodeRegistryAuth(t *testing.T, s string) registry.AuthConfig {
	t.Helper()
	raw, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode RegistryAuth: %v", err)
	}
	var cfg registry.AuthConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal AuthConfig: %v", err)
	}
	return cfg
}

func TestRegistryAuthFor_DockerHubLegacyKey(t *testing.T) {
	dir := t.TempDir()
	// "alice:s3cret" under the legacy index URL the CLI writes.
	writeConfig(t, dir, `{
	  "auths": {
	    "https://index.docker.io/v1/": {"auth": "YWxpY2U6czNjcmV0"}
	  }
	}`)
	t.Setenv("DOCKER_CONFIG", dir)

	got, err := RegistryAuthFor("alpine:3.23")
	if err != nil {
		t.Fatalf("RegistryAuthFor: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty RegistryAuth for docker.io image")
	}
	cfg := decodeRegistryAuth(t, got)
	if cfg.Username != "alice" || cfg.Password != "s3cret" {
		t.Errorf("got username=%q password=%q, want alice/s3cret", cfg.Username, cfg.Password)
	}
}

func TestRegistryAuthFor_ExplicitDockerIoDomain(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{
	  "auths": {
	    "docker.io": {"auth": "Ym9iOnRva2Vu"}
	  }
	}`)
	t.Setenv("DOCKER_CONFIG", dir)

	got, err := RegistryAuthFor("docker.io/library/postgres:16")
	if err != nil {
		t.Fatalf("RegistryAuthFor: %v", err)
	}
	cfg := decodeRegistryAuth(t, got)
	if cfg.Username != "bob" || cfg.Password != "token" {
		t.Errorf("got %q/%q, want bob/token", cfg.Username, cfg.Password)
	}
}

func TestRegistryAuthFor_OtherRegistry(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{
	  "auths": {
	    "ghcr.io": {"auth": "Z2g6cGF0"}
	  }
	}`)
	t.Setenv("DOCKER_CONFIG", dir)

	got, err := RegistryAuthFor("ghcr.io/cynkra/blockyard:latest")
	if err != nil {
		t.Fatalf("RegistryAuthFor: %v", err)
	}
	cfg := decodeRegistryAuth(t, got)
	if cfg.Username != "gh" || cfg.Password != "pat" {
		t.Errorf("got %q/%q, want gh/pat", cfg.Username, cfg.Password)
	}
	if cfg.ServerAddress != "ghcr.io" {
		t.Errorf("ServerAddress = %q, want ghcr.io", cfg.ServerAddress)
	}
}

func TestRegistryAuthFor_NoMatchReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{
	  "auths": {
	    "ghcr.io": {"auth": "Z2g6cGF0"}
	  }
	}`)
	t.Setenv("DOCKER_CONFIG", dir)

	got, err := RegistryAuthFor("alpine:3.23")
	if err != nil {
		t.Fatalf("RegistryAuthFor: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty auth for unmatched domain, got %q", got)
	}
}

func TestRegistryAuthFor_MissingConfig(t *testing.T) {
	dir := t.TempDir() // empty
	t.Setenv("DOCKER_CONFIG", dir)

	got, err := RegistryAuthFor("alpine:3.23")
	if err != nil {
		t.Fatalf("RegistryAuthFor: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty auth when config is missing, got %q", got)
	}
}

func TestRegistryAuthFor_IdentityTokenOnly(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{
	  "auths": {
	    "ghcr.io": {"identitytoken": "opaque-refresh-token"}
	  }
	}`)
	t.Setenv("DOCKER_CONFIG", dir)

	got, err := RegistryAuthFor("ghcr.io/foo/bar:1")
	if err != nil {
		t.Fatalf("RegistryAuthFor: %v", err)
	}
	cfg := decodeRegistryAuth(t, got)
	if cfg.IdentityToken != "opaque-refresh-token" {
		t.Errorf("IdentityToken = %q, want opaque-refresh-token", cfg.IdentityToken)
	}
	if cfg.Username != "" || cfg.Password != "" {
		t.Errorf("expected no username/password, got %q/%q", cfg.Username, cfg.Password)
	}
}

func TestRegistryAuthFor_MalformedRef(t *testing.T) {
	if _, err := RegistryAuthFor("::not a ref::"); err == nil {
		t.Fatal("expected error for malformed ref, got nil")
	}
}

func TestRegistryAuthFor_CorruptConfig(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{not json`)
	t.Setenv("DOCKER_CONFIG", dir)

	if _, err := RegistryAuthFor("alpine:3.23"); err == nil {
		t.Fatal("expected error for corrupt config, got nil")
	}
}
