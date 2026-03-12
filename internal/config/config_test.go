package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
	if cfg.Server.Token.Expose() != "test-token" {
		t.Errorf("expected test-token, got %q", cfg.Server.Token.Expose())
	}
	if cfg.Proxy.MaxWorkers != 100 {
		t.Errorf("expected default max_workers 100, got %d", cfg.Proxy.MaxWorkers)
	}
}

func TestEnvVarOverridesToken(t *testing.T) {
	t.Setenv("BLOCKYARD_SERVER_TOKEN", "override-token")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Server.Token.Expose() != "override-token" {
		t.Errorf("expected override-token, got %q", cfg.Server.Token.Expose())
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

		ft := f.Type
		// Dereference pointer types.
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}

		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(Duration{}) && ft != reflect.TypeOf(Secret{}) {
			names = append(names, collectEnvVarNames(ft, envName)...)
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
	// This will fail on validation (non-writable /data/bundles) but shows the default was set.
	_, err := Load(path)
	if err == nil {
		t.Error("expected error for non-writable default path")
	}
	if err != nil && !strings.Contains(err.Error(), "bundle_server_path") {
		t.Errorf("expected error about bundle_server_path, got: %v", err)
	}
}

func TestDefaultDatabasePath(t *testing.T) {
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
	// This will fail on validation (non-writable /data/db) but shows the default was set.
	_, err := Load(path)
	if err == nil {
		t.Error("expected error for non-writable default db path")
	}
	if err != nil && !strings.Contains(err.Error(), "database.path") {
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

// --- Secret type tests ---

func TestSecretStringRedacts(t *testing.T) {
	s := NewSecret("super-secret")
	if s.String() != "[REDACTED]" {
		t.Errorf("String() = %q, want [REDACTED]", s.String())
	}
	if fmt.Sprintf("%#v", s) != "[REDACTED]" {
		t.Errorf("GoString() = %q, want [REDACTED]", fmt.Sprintf("%#v", s))
	}
}

func TestSecretExpose(t *testing.T) {
	s := NewSecret("my-value")
	if s.Expose() != "my-value" {
		t.Errorf("Expose() = %q, want my-value", s.Expose())
	}
}

func TestSecretIsEmpty(t *testing.T) {
	if !NewSecret("").IsEmpty() {
		t.Error("expected empty secret to be empty")
	}
	if NewSecret("x").IsEmpty() {
		t.Error("expected non-empty secret to not be empty")
	}
}

func TestSecretUnmarshalFromTOML(t *testing.T) {
	cfg := loadFromString(t, minimalTOML)
	if cfg.Server.Token.Expose() != "test-token" {
		t.Errorf("Token not deserialized: got %q", cfg.Server.Token.Expose())
	}
}

// --- OIDC config tests ---

func oidcTOML(t *testing.T) string {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	return fmt.Sprintf(`
[server]
token = "test-token"
session_secret = "my-session-secret"
external_url = "https://example.com"

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
path = %q

[proxy]

[oidc]
issuer_url = "https://idp.example.com"
client_id = "my-client"
client_secret = "my-secret"
`, bundlePath, dbPath)
}

func TestParseOidcConfig(t *testing.T) {
	cfg := loadFromString(t, oidcTOML(t))
	if cfg.OIDC == nil {
		t.Fatal("expected OIDC config to be parsed")
	}
	if cfg.OIDC.IssuerURL != "https://idp.example.com" {
		t.Errorf("issuer_url = %q", cfg.OIDC.IssuerURL)
	}
	if cfg.OIDC.ClientID != "my-client" {
		t.Errorf("client_id = %q", cfg.OIDC.ClientID)
	}
	if cfg.OIDC.ClientSecret.Expose() != "my-secret" {
		t.Errorf("client_secret = %q", cfg.OIDC.ClientSecret.Expose())
	}
	if cfg.OIDC.GroupsClaim != "groups" {
		t.Errorf("expected default groups_claim 'groups', got %q", cfg.OIDC.GroupsClaim)
	}
	if cfg.OIDC.CookieMaxAge.Duration != 24*time.Hour {
		t.Errorf("expected default cookie_max_age 24h, got %v", cfg.OIDC.CookieMaxAge.Duration)
	}
}

func TestParseConfigWithoutOidc(t *testing.T) {
	cfg := loadFromString(t, minimalTOML)
	if cfg.OIDC != nil {
		t.Error("expected OIDC config to be nil when section is absent")
	}
}

func TestValidationRejectsOidcEmptyIssuerURL(t *testing.T) {
	toml := strings.Replace(oidcTOML(t), `issuer_url = "https://idp.example.com"`, `issuer_url = ""`, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "oidc.issuer_url") {
		t.Errorf("expected oidc.issuer_url error, got: %v", err)
	}
}

func TestValidationRejectsOidcEmptyClientID(t *testing.T) {
	toml := strings.Replace(oidcTOML(t), `client_id = "my-client"`, `client_id = ""`, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "oidc.client_id") {
		t.Errorf("expected oidc.client_id error, got: %v", err)
	}
}

func TestValidationRejectsOidcEmptyClientSecret(t *testing.T) {
	toml := strings.Replace(oidcTOML(t), `client_secret = "my-secret"`, `client_secret = ""`, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "oidc.client_secret") {
		t.Errorf("expected oidc.client_secret error, got: %v", err)
	}
}

func TestValidationRejectsOidcWithoutSessionSecret(t *testing.T) {
	toml := strings.Replace(oidcTOML(t), `session_secret = "my-session-secret"`, ``, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "session_secret") {
		t.Errorf("expected session_secret error, got: %v", err)
	}
}

func TestEnvVarOverrideOidcFields(t *testing.T) {
	t.Setenv("BLOCKYARD_OIDC_GROUPS_CLAIM", "roles")
	t.Setenv("BLOCKYARD_OIDC_COOKIE_MAX_AGE", "12h")
	cfg := loadFromString(t, oidcTOML(t))
	if cfg.OIDC.GroupsClaim != "roles" {
		t.Errorf("groups_claim = %q, want 'roles'", cfg.OIDC.GroupsClaim)
	}
	if cfg.OIDC.CookieMaxAge.Duration != 12*time.Hour {
		t.Errorf("cookie_max_age = %v, want 12h", cfg.OIDC.CookieMaxAge.Duration)
	}
}

func TestOidcAutoConstructFromEnvVars(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	toml := fmt.Sprintf(`
[server]
token = "test-token"
session_secret = "my-session-secret"

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
path = %q

[proxy]
`, bundlePath, dbPath)

	t.Setenv("BLOCKYARD_OIDC_ISSUER_URL", "https://env-idp.example.com")
	t.Setenv("BLOCKYARD_OIDC_CLIENT_ID", "env-client")
	t.Setenv("BLOCKYARD_OIDC_CLIENT_SECRET", "env-secret")

	cfg := loadFromString(t, toml)
	if cfg.OIDC == nil {
		t.Fatal("expected OIDC section to be auto-constructed from env vars")
	}
	if cfg.OIDC.IssuerURL != "https://env-idp.example.com" {
		t.Errorf("issuer_url = %q", cfg.OIDC.IssuerURL)
	}
	if cfg.OIDC.ClientID != "env-client" {
		t.Errorf("client_id = %q", cfg.OIDC.ClientID)
	}
	if cfg.OIDC.ClientSecret.Expose() != "env-secret" {
		t.Errorf("client_secret = %q", cfg.OIDC.ClientSecret.Expose())
	}
}

func TestEnvVarOverrideSessionSecret(t *testing.T) {
	t.Setenv("BLOCKYARD_SERVER_SESSION_SECRET", "env-session-secret")
	cfg := loadFromString(t, oidcTOML(t))
	if cfg.Server.SessionSecret == nil {
		t.Fatal("expected SessionSecret to be set")
	}
	if cfg.Server.SessionSecret.Expose() != "env-session-secret" {
		t.Errorf("session_secret = %q", cfg.Server.SessionSecret.Expose())
	}
}

func TestEnvVarOverrideExternalURL(t *testing.T) {
	t.Setenv("BLOCKYARD_SERVER_EXTERNAL_URL", "https://override.example.com")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Server.ExternalURL != "https://override.example.com" {
		t.Errorf("external_url = %q", cfg.Server.ExternalURL)
	}
}

// --- OpenBao config tests ---

func openbaoTOML(t *testing.T) string {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	return fmt.Sprintf(`
[server]
token = "test-token"
session_secret = "my-session-secret"

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
path = %q

[proxy]

[oidc]
issuer_url = "https://idp.example.com"
client_id = "my-client"
client_secret = "my-secret"

[openbao]
address = "https://bao.example.com"
admin_token = "hvs.admin123"
`, bundlePath, dbPath)
}

func TestParseOpenbaoConfig(t *testing.T) {
	cfg := loadFromString(t, openbaoTOML(t))
	if cfg.Openbao == nil {
		t.Fatal("expected Openbao config to be parsed")
	}
	if cfg.Openbao.Address != "https://bao.example.com" {
		t.Errorf("address = %q", cfg.Openbao.Address)
	}
	if cfg.Openbao.AdminToken.Expose() != "hvs.admin123" {
		t.Errorf("admin_token = %q", cfg.Openbao.AdminToken.Expose())
	}
	if cfg.Openbao.TokenTTL.Duration != 1*time.Hour {
		t.Errorf("expected default token_ttl 1h, got %v", cfg.Openbao.TokenTTL.Duration)
	}
	if cfg.Openbao.JWTAuthPath != "jwt" {
		t.Errorf("expected default jwt_auth_path 'jwt', got %q", cfg.Openbao.JWTAuthPath)
	}
}

func TestParseConfigWithoutOpenbao(t *testing.T) {
	cfg := loadFromString(t, minimalTOML)
	if cfg.Openbao != nil {
		t.Error("expected Openbao config to be nil when section is absent")
	}
}

func TestValidationRejectsOpenbaoEmptyAddress(t *testing.T) {
	toml := strings.Replace(openbaoTOML(t), `address = "https://bao.example.com"`, `address = ""`, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "openbao.address") {
		t.Errorf("expected openbao.address error, got: %v", err)
	}
}

func TestValidationRejectsOpenbaoEmptyAdminToken(t *testing.T) {
	toml := strings.Replace(openbaoTOML(t), `admin_token = "hvs.admin123"`, `admin_token = ""`, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "openbao.admin_token") {
		t.Errorf("expected openbao.admin_token error, got: %v", err)
	}
}

func TestValidationRejectsOpenbaoWithoutOidc(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	toml := fmt.Sprintf(`
[server]
token = "test-token"

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
path = %q

[openbao]
address = "https://bao.example.com"
admin_token = "hvs.admin123"
`, bundlePath, dbPath)

	cfgDir := t.TempDir()
	path := filepath.Join(cfgDir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "[oidc] is required") {
		t.Errorf("expected '[oidc] is required' error, got: %v", err)
	}
}

func TestOpenbaoAutoConstructFromEnvVars(t *testing.T) {
	t.Setenv("BLOCKYARD_OPENBAO_ADDRESS", "https://env-bao.example.com")
	t.Setenv("BLOCKYARD_OPENBAO_ADMIN_TOKEN", "hvs.env-token")
	// Also need OIDC for openbao validation to pass.
	t.Setenv("BLOCKYARD_OIDC_ISSUER_URL", "https://idp.example.com")
	t.Setenv("BLOCKYARD_OIDC_CLIENT_ID", "my-client")
	t.Setenv("BLOCKYARD_OIDC_CLIENT_SECRET", "my-secret")
	t.Setenv("BLOCKYARD_SERVER_SESSION_SECRET", "my-session-secret")

	cfg := loadFromString(t, minimalTOML)
	if cfg.Openbao == nil {
		t.Fatal("expected Openbao section to be auto-constructed from env vars")
	}
	if cfg.Openbao.Address != "https://env-bao.example.com" {
		t.Errorf("address = %q", cfg.Openbao.Address)
	}
	if cfg.Openbao.AdminToken.Expose() != "hvs.env-token" {
		t.Errorf("admin_token = %q", cfg.Openbao.AdminToken.Expose())
	}
}

func TestEnvVarOverrideOpenbaoTokenTTL(t *testing.T) {
	t.Setenv("BLOCKYARD_OPENBAO_TOKEN_TTL", "30m")
	cfg := loadFromString(t, openbaoTOML(t))
	if cfg.Openbao.TokenTTL.Duration != 30*time.Minute {
		t.Errorf("token_ttl = %v, want 30m", cfg.Openbao.TokenTTL.Duration)
	}
}
