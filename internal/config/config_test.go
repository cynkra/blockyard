package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const minimalTOML = `
[server]
token = "test-token"

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"

[proxy]
`

func loadFromString(t *testing.T, content string) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestParseMinimalConfig(t *testing.T) {
	cfg := loadFromString(t, minimalTOML)
	if cfg.Server.Bind != "0.0.0.0:8080" {
		t.Errorf("expected default bind, got %q", cfg.Server.Bind)
	}
	if cfg.Server.Token != "test-token" {
		t.Errorf("expected test-token, got %q", cfg.Server.Token)
	}
	if cfg.Proxy.MaxWorkers != 100 {
		t.Errorf("expected default max_workers 100, got %d", cfg.Proxy.MaxWorkers)
	}
}

func TestEnvVarOverridesToken(t *testing.T) {
	t.Setenv("BLOCKYARD_SERVER_TOKEN", "override-token")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Server.Token != "override-token" {
		t.Errorf("expected override-token, got %q", cfg.Server.Token)
	}
}

func TestValidationRejectsEmptyToken(t *testing.T) {
	tomlContent := `
[server]
token = ""

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"

[proxy]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(tomlContent), 0o644)
	_, err := Load(path)
	if err == nil {
		t.Error("expected validation error for empty token")
	}
}

// collectEnvVarNames walks Config struct tags and returns all derived
// env var names. Used by tests only.
func collectEnvVarNames(t reflect.Type, prefix string) []string {
	var names []string
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		envName := prefix + "_" + strings.ToUpper(tag)
		if f.Type.Kind() == reflect.Struct && f.Type != reflect.TypeOf(Duration{}) {
			names = append(names, collectEnvVarNames(f.Type, envName)...)
		} else {
			names = append(names, envName)
		}
	}
	return names
}

func TestEnvVarNamesUnique(t *testing.T) {
	names := collectEnvVarNames(reflect.TypeOf(Config{}), "BLOCKYARD")
	seen := make(map[string]bool)
	for _, name := range names {
		if seen[name] {
			t.Errorf("duplicate env var name: %s", name)
		}
		seen[name] = true
	}
}
