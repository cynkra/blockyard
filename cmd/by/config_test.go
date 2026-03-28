package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigSaveLoad(t *testing.T) {
	// Use a temp directory for XDG_CONFIG_HOME.
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	cfg := &config{
		Server: "https://example.com",
		Token:  "by_testtoken123",
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	// Verify file permissions.
	info, err := os.Stat(filepath.Join(tmp, "by", "config.json"))
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected 0600 perms, got %o", info.Mode().Perm())
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if loaded.Server != cfg.Server {
		t.Errorf("server: got %q, want %q", loaded.Server, cfg.Server)
	}
	if loaded.Token != cfg.Token {
		t.Errorf("token: got %q, want %q", loaded.Token, cfg.Token)
	}
}

func TestConfigLoadMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Server != "" || cfg.Token != "" {
		t.Errorf("expected empty config, got %+v", cfg)
	}
}

func TestResolveCredentials_EnvVarsPrecedence(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	// Save config file.
	cfg := &config{
		Server: "https://file.example.com",
		Token:  "by_filetoken",
	}
	saveConfig(cfg)

	// Env vars should take precedence.
	t.Setenv("BLOCKYARD_URL", "https://env.example.com")
	t.Setenv("BLOCKYARD_TOKEN", "by_envtoken")

	serverURL, token, err := resolveCredentials()
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if serverURL != "https://env.example.com" {
		t.Errorf("server: got %q, want env value", serverURL)
	}
	if token != "by_envtoken" {
		t.Errorf("token: got %q, want env value", token)
	}
}

func TestResolveCredentials_FallbackToFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("BLOCKYARD_URL", "")
	t.Setenv("BLOCKYARD_TOKEN", "")

	cfg := &config{
		Server: "https://file.example.com",
		Token:  "by_filetoken",
	}
	saveConfig(cfg)

	serverURL, token, err := resolveCredentials()
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if serverURL != "https://file.example.com" {
		t.Errorf("server: got %q, want file value", serverURL)
	}
	if token != "by_filetoken" {
		t.Errorf("token: got %q, want file value", token)
	}
}

func TestResolveCredentials_NoConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("BLOCKYARD_URL", "")
	t.Setenv("BLOCKYARD_TOKEN", "")

	_, _, err := resolveCredentials()
	if err == nil {
		t.Fatal("expected error when no config exists")
	}
}
