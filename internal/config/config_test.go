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

func TestEnvVarOverridesDockerImage(t *testing.T) {
	t.Setenv("BLOCKYARD_DOCKER_IMAGE", "custom-image:v2")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Docker.Image != "custom-image:v2" {
		t.Errorf("expected custom-image:v2, got %q", cfg.Docker.Image)
	}
}

func TestEnvVarOverridesMaxWorkers(t *testing.T) {
	t.Setenv("BLOCKYARD_PROXY_MAX_WORKERS", "42")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Proxy.MaxWorkers != 42 {
		t.Errorf("expected 42, got %d", cfg.Proxy.MaxWorkers)
	}
}

func TestEnvVarOverridesWsCacheTTL(t *testing.T) {
	t.Setenv("BLOCKYARD_PROXY_WS_CACHE_TTL", "5m")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Proxy.WsCacheTTL.Duration != 5*60*1000000000 { // 5 minutes
		t.Errorf("expected 5m, got %v", cfg.Proxy.WsCacheTTL.Duration)
	}
}

func TestValidationRejectsEmptyImage(t *testing.T) {
	tomlContent := `
[server]
token = "test-token"

[docker]
image = ""

[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(tomlContent), 0o644)
	_, err := Load(path)
	if err == nil {
		t.Error("expected validation error for empty image")
	}
	if err != nil && !strings.Contains(err.Error(), "docker.image") {
		t.Errorf("expected error about docker.image, got: %v", err)
	}
}

func TestDefaultBundleServerPath(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db", "blockyard.db")
	bundlePath := filepath.Join(tmpDir, "bundles")
	tomlContent := `
[server]
token = "test-token"

[docker]
image = "some-image"

[database]
path = "` + dbPath + `"

[storage]
bundle_server_path = "` + bundlePath + `"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(tomlContent), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Storage.BundleServerPath != bundlePath {
		t.Errorf("expected bundle_server_path %q, got %q", bundlePath, cfg.Storage.BundleServerPath)
	}
}

func TestDefaultBundleServerPathUsesDataBundles(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db", "blockyard.db")
	tomlContent := `
[server]
token = "test-token"

[docker]
image = "some-image"

[database]
path = "` + dbPath + `"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(tomlContent), 0o644)
	// Load may fail if /data/bundles is not writable, but we can
	// verify the default was applied by checking the error message.
	cfg, err := Load(path)
	if err == nil {
		// /data/bundles happened to be writable (e.g. running as root)
		if cfg.Storage.BundleServerPath != "/data/bundles" {
			t.Errorf("expected default /data/bundles, got %q", cfg.Storage.BundleServerPath)
		}
	} else if !strings.Contains(err.Error(), "bundle_server_path") {
		t.Errorf("expected error about bundle_server_path, got: %v", err)
	}
}

func TestDefaultDatabasePath(t *testing.T) {
	tmpDir := t.TempDir()
	bundlePath := filepath.Join(tmpDir, "bundles")
	dbPath := filepath.Join(tmpDir, "db", "blockyard.db")
	tomlContent := `
[server]
token = "test-token"

[docker]
image = "some-image"

[storage]
bundle_server_path = "` + bundlePath + `"

[database]
path = "` + dbPath + `"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(tomlContent), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Database.Path != dbPath {
		t.Errorf("expected database.path %q, got %q", dbPath, cfg.Database.Path)
	}
}

func TestDefaultDatabasePathUsesDataDb(t *testing.T) {
	tmpDir := t.TempDir()
	bundlePath := filepath.Join(tmpDir, "bundles")
	tomlContent := `
[server]
token = "test-token"

[docker]
image = "some-image"

[storage]
bundle_server_path = "` + bundlePath + `"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(tomlContent), 0o644)
	cfg, err := Load(path)
	if err == nil {
		if cfg.Database.Path != "/data/db/blockyard.db" {
			t.Errorf("expected default /data/db/blockyard.db, got %q", cfg.Database.Path)
		}
	} else if !strings.Contains(err.Error(), "database.path") {
		t.Errorf("expected error about database.path, got: %v", err)
	}
}

func TestValidationRejectsNonWritableBundlePath(t *testing.T) {
	tomlContent := `
[server]
token = "test-token"

[docker]
image = "some-image"

[storage]
bundle_server_path = "/proc/nonexistent/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(tomlContent), 0o644)
	_, err := Load(path)
	if err == nil {
		t.Error("expected validation error for non-writable bundle path")
	}
	if err != nil && !strings.Contains(err.Error(), "bundle_server_path") {
		t.Errorf("expected error about bundle_server_path, got: %v", err)
	}
}

func TestValidationRejectsNonWritableDatabaseDir(t *testing.T) {
	tmpDir := t.TempDir()
	bundlePath := filepath.Join(tmpDir, "bundles")
	tomlContent := `
[server]
token = "test-token"

[docker]
image = "some-image"

[storage]
bundle_server_path = "` + bundlePath + `"

[database]
path = "/proc/nonexistent/db/blockyard.db"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(tomlContent), 0o644)
	_, err := Load(path)
	if err == nil {
		t.Error("expected validation error for non-writable database dir")
	}
	if err != nil && !strings.Contains(err.Error(), "database.path parent directory") {
		t.Errorf("expected error about database.path parent directory, got: %v", err)
	}
}

func TestDurationUnmarshalText(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("5m")); err != nil {
		t.Fatalf("UnmarshalText: %v", err)
	}
	if d.Duration != 5*60*1000000000 {
		t.Errorf("expected 5m, got %v", d.Duration)
	}

	// Invalid duration
	if err := d.UnmarshalText([]byte("not-a-duration")); err == nil {
		t.Error("expected error for invalid duration")
	}
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
