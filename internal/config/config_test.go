package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const minimalTOML = `
[server]


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
	if cfg.Proxy.MaxWorkers != 100 {
		t.Errorf("expected default max_workers 100, got %d", cfg.Proxy.MaxWorkers)
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

func TestEnvVarOverridesMaxCPULimit(t *testing.T) {
	t.Setenv("BLOCKYARD_PROXY_MAX_CPU_LIMIT", "8.5")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Proxy.MaxCPULimit == nil {
		t.Fatal("expected MaxCPULimit to be set")
	}
	if *cfg.Proxy.MaxCPULimit != 8.5 {
		t.Errorf("expected 8.5, got %f", *cfg.Proxy.MaxCPULimit)
	}
}

func TestEnvVarOverridesMaxCPULimitZero(t *testing.T) {
	t.Setenv("BLOCKYARD_PROXY_MAX_CPU_LIMIT", "0")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Proxy.MaxCPULimit == nil {
		t.Fatal("expected MaxCPULimit to be set")
	}
	if *cfg.Proxy.MaxCPULimit != 0 {
		t.Errorf("expected 0, got %f", *cfg.Proxy.MaxCPULimit)
	}
}

func TestValidationRejectsEmptyImage(t *testing.T) {
	tomlContent := `
[server]


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
	v, err := s.Expose()
	if err != nil {
		t.Fatal(err)
	}
	if v != "my-value" {
		t.Errorf("Expose() = %q, want my-value", v)
	}
}

func TestSecretExposeRejectsUnresolvedVaultRef(t *testing.T) {
	s := NewSecret("vault:secret/data/foo#bar")
	_, err := s.Expose()
	if err == nil {
		t.Fatal("expected error for unresolved vault reference")
	}
	if !strings.Contains(err.Error(), "unresolved vault reference") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSecretMustExpose(t *testing.T) {
	s := NewSecret("my-value")
	if s.MustExpose() != "my-value" {
		t.Errorf("MustExpose() = %q, want my-value", s.MustExpose())
	}
}

func TestSecretMustExposePanicsOnVaultRef(t *testing.T) {
	s := NewSecret("vault:secret/data/foo#bar")
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for unresolved vault reference")
		}
	}()
	s.MustExpose()
}

func TestSecretIsVaultRef(t *testing.T) {
	if !NewSecret("vault:secret/data/foo#bar").IsVaultRef() {
		t.Error("expected IsVaultRef() = true")
	}
	if NewSecret("plain-value").IsVaultRef() {
		t.Error("expected IsVaultRef() = false")
	}
}

func TestSecretSetValue(t *testing.T) {
	s := NewSecret("")
	s.SetValue("new-value")
	if s.MustExpose() != "new-value" {
		t.Errorf("after SetValue: got %q", s.MustExpose())
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
	cfg := loadFromString(t, oidcTOML(t))
	if cfg.OIDC.ClientSecret.MustExpose() != "my-secret" {
		t.Errorf("Secret not deserialized: got %q", cfg.OIDC.ClientSecret.MustExpose())
	}
}

// --- OIDC config tests ---

func oidcTOML(t *testing.T) string {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	return fmt.Sprintf(`
[server]

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
	if cfg.OIDC.ClientSecret.MustExpose() != "my-secret" {
		t.Errorf("client_secret = %q", cfg.OIDC.ClientSecret.MustExpose())
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
	// Without [openbao], session_secret is required.
	toml := strings.Replace(oidcTOML(t), `session_secret = "my-session-secret"`, ``, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "session_secret") {
		t.Errorf("expected session_secret error, got: %v", err)
	}
}

func TestValidationDefersSessionSecretWithOpenbao(t *testing.T) {
	// With [openbao] configured, session_secret is not required at load time
	// (it may be auto-generated or resolved from vault).
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	toml := fmt.Sprintf(`
[server]

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
path = %q

[oidc]
issuer_url = "https://idp.example.com"
client_id = "my-client"
client_secret = "my-secret"

[openbao]
address = "https://bao.example.com"
role_id = "blockyard-server"
`, bundlePath, dbPath)
	cfgDir := t.TempDir()
	path := filepath.Join(cfgDir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err != nil {
		t.Errorf("expected no error (session_secret deferred), got: %v", err)
	}
}

func TestValidationRejectsBothAdminTokenAndRoleID(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	toml := fmt.Sprintf(`
[server]
session_secret = "my-session-secret"

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
path = %q

[oidc]
issuer_url = "https://idp.example.com"
client_id = "my-client"
client_secret = "my-secret"

[openbao]
address = "https://bao.example.com"
admin_token = "hvs.admin123"
role_id = "blockyard-server"
`, bundlePath, dbPath)
	cfgDir := t.TempDir()
	path := filepath.Join(cfgDir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutually exclusive error, got: %v", err)
	}
}

func TestParseOpenbaoRoleID(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	toml := fmt.Sprintf(`
[server]
session_secret = "my-session-secret"

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
path = %q

[oidc]
issuer_url = "https://idp.example.com"
client_id = "my-client"
client_secret = "my-secret"

[openbao]
address = "https://bao.example.com"
role_id = "blockyard-server"
`, bundlePath, dbPath)
	cfg := loadFromString(t, toml)
	if cfg.Openbao == nil {
		t.Fatal("expected Openbao config")
	}
	if cfg.Openbao.RoleID != "blockyard-server" {
		t.Errorf("role_id = %q, want blockyard-server", cfg.Openbao.RoleID)
	}
	if !cfg.Openbao.AdminToken.IsEmpty() {
		t.Error("expected admin_token to be empty")
	}
}

func TestEnvVarOverrideOidcFields(t *testing.T) {
	t.Setenv("BLOCKYARD_OIDC_COOKIE_MAX_AGE", "12h")
	cfg := loadFromString(t, oidcTOML(t))
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
	if cfg.OIDC.ClientSecret.MustExpose() != "env-secret" {
		t.Errorf("client_secret = %q", cfg.OIDC.ClientSecret.MustExpose())
	}
}

func TestEnvVarOverrideManagementBind(t *testing.T) {
	t.Setenv("BLOCKYARD_SERVER_MANAGEMENT_BIND", "127.0.0.1:9100")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Server.ManagementBind != "127.0.0.1:9100" {
		t.Errorf("management_bind = %q, want 127.0.0.1:9100", cfg.Server.ManagementBind)
	}
}

func TestManagementBindFromTOML(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	toml := fmt.Sprintf(`
[server]
management_bind = "127.0.0.1:9100"

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
path = %q

[proxy]
`, bundlePath, dbPath)
	cfg := loadFromString(t, toml)
	if cfg.Server.ManagementBind != "127.0.0.1:9100" {
		t.Errorf("management_bind = %q, want 127.0.0.1:9100", cfg.Server.ManagementBind)
	}
}

func TestEnvVarOverrideSessionSecret(t *testing.T) {
	t.Setenv("BLOCKYARD_SERVER_SESSION_SECRET", "env-session-secret")
	cfg := loadFromString(t, oidcTOML(t))
	if cfg.Server.SessionSecret == nil {
		t.Fatal("expected SessionSecret to be set")
	}
	if cfg.Server.SessionSecret.MustExpose() != "env-session-secret" {
		t.Errorf("session_secret = %q", cfg.Server.SessionSecret.MustExpose())
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
	if cfg.Openbao.AdminToken.MustExpose() != "hvs.admin123" {
		t.Errorf("admin_token = %q", cfg.Openbao.AdminToken.MustExpose())
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

func TestValidationRejectsOpenbaoNoCredentials(t *testing.T) {
	toml := strings.Replace(openbaoTOML(t), `admin_token = "hvs.admin123"`, `admin_token = ""`, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "requires either admin_token or role_id") {
		t.Errorf("expected 'requires either admin_token or role_id' error, got: %v", err)
	}
}

func TestValidationRejectsOpenbaoWithoutOidc(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	toml := fmt.Sprintf(`
[server]


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
	if cfg.Openbao.AdminToken.MustExpose() != "hvs.env-token" {
		t.Errorf("admin_token = %q", cfg.Openbao.AdminToken.MustExpose())
	}
}

func TestEnvVarOverrideOpenbaoTokenTTL(t *testing.T) {
	t.Setenv("BLOCKYARD_OPENBAO_TOKEN_TTL", "30m")
	cfg := loadFromString(t, openbaoTOML(t))
	if cfg.Openbao.TokenTTL.Duration != 30*time.Minute {
		t.Errorf("token_ttl = %v, want 30m", cfg.Openbao.TokenTTL.Duration)
	}
}

// --- Audit config tests ---

func auditTOML(t *testing.T) string {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	auditPath := filepath.Join(dir, "audit.jsonl")
	return fmt.Sprintf(`
[server]


[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
path = %q

[proxy]

[audit]
path = %q
`, bundlePath, dbPath, auditPath)
}

func TestParseAuditConfig(t *testing.T) {
	cfg := loadFromString(t, auditTOML(t))
	if cfg.Audit == nil {
		t.Fatal("expected Audit config to be parsed")
	}
	if cfg.Audit.Path == "" {
		t.Error("expected non-empty audit path")
	}
}

func TestParseConfigWithoutAudit(t *testing.T) {
	cfg := loadFromString(t, minimalTOML)
	if cfg.Audit != nil {
		t.Error("expected Audit config to be nil when section is absent")
	}
}

func TestValidationRejectsAuditEmptyPath(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	toml := fmt.Sprintf(`
[server]


[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
path = %q

[audit]
path = ""
`, bundlePath, dbPath)
	cfgDir := t.TempDir()
	path := filepath.Join(cfgDir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "audit.path") {
		t.Errorf("expected audit.path error, got: %v", err)
	}
}

func TestAuditAutoConstructFromEnvVars(t *testing.T) {
	t.Setenv("BLOCKYARD_AUDIT_PATH", "/tmp/audit.jsonl")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Audit == nil {
		t.Fatal("expected Audit section to be auto-constructed from env vars")
	}
	if cfg.Audit.Path != "/tmp/audit.jsonl" {
		t.Errorf("path = %q", cfg.Audit.Path)
	}
}

// --- Database driver config tests ---

func TestDatabaseDriverDefaultsSQLite(t *testing.T) {
	cfg := loadFromString(t, minimalTOML)
	if cfg.Database.Driver != "sqlite" {
		t.Errorf("expected default driver sqlite, got %q", cfg.Database.Driver)
	}
}

func TestValidPostgresConfig(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	toml := fmt.Sprintf(`
[server]

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
driver = "postgres"
url = "postgres://blockyard:blockyard@localhost:5432/blockyard?sslmode=disable"
`, bundlePath)
	cfg := loadFromString(t, toml)
	if cfg.Database.Driver != "postgres" {
		t.Errorf("expected driver postgres, got %q", cfg.Database.Driver)
	}
	if cfg.Database.URL == "" {
		t.Error("expected non-empty database URL")
	}
}

func TestValidationRejectsPostgresWithoutURL(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	toml := fmt.Sprintf(`
[server]

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
driver = "postgres"
`, bundlePath)
	cfgDir := t.TempDir()
	path := filepath.Join(cfgDir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "database.url is required") {
		t.Errorf("expected database.url error, got: %v", err)
	}
}

func TestValidationRejectsUnsupportedDriver(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	toml := fmt.Sprintf(`
[server]

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
driver = "banana"
`, bundlePath)
	cfgDir := t.TempDir()
	path := filepath.Join(cfgDir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `"banana"`) {
		t.Errorf("expected error about unsupported driver, got: %v", err)
	}
}

func TestEnvVarOverrideDatabaseDriver(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	toml := fmt.Sprintf(`
[server]

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
`, bundlePath)

	t.Setenv("BLOCKYARD_DATABASE_DRIVER", "postgres")
	t.Setenv("BLOCKYARD_DATABASE_URL", "postgres://blockyard:blockyard@localhost:5432/blockyard?sslmode=disable")

	cfg := loadFromString(t, toml)
	if cfg.Database.Driver != "postgres" {
		t.Errorf("expected driver postgres from env, got %q", cfg.Database.Driver)
	}
	if cfg.Database.URL != "postgres://blockyard:blockyard@localhost:5432/blockyard?sslmode=disable" {
		t.Errorf("expected URL from env, got %q", cfg.Database.URL)
	}
}

// --- Telemetry config tests ---

func telemetryTOML(t *testing.T) string {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	return fmt.Sprintf(`
[server]


[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
path = %q

[proxy]

[telemetry]
metrics_enabled = true
otlp_endpoint = "localhost:4317"
`, bundlePath, dbPath)
}

func TestParseTelemetryConfig(t *testing.T) {
	cfg := loadFromString(t, telemetryTOML(t))
	if cfg.Telemetry == nil {
		t.Fatal("expected Telemetry config to be parsed")
	}
	if !cfg.Telemetry.MetricsEnabled {
		t.Error("expected metrics_enabled = true")
	}
	if cfg.Telemetry.OTLPEndpoint != "localhost:4317" {
		t.Errorf("otlp_endpoint = %q", cfg.Telemetry.OTLPEndpoint)
	}
}

func TestParseConfigWithoutTelemetry(t *testing.T) {
	cfg := loadFromString(t, minimalTOML)
	if cfg.Telemetry != nil {
		t.Error("expected Telemetry config to be nil when section is absent")
	}
}

func TestTelemetryAutoConstructFromEnvVars(t *testing.T) {
	t.Setenv("BLOCKYARD_TELEMETRY_METRICS_ENABLED", "true")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Telemetry == nil {
		t.Fatal("expected Telemetry section to be auto-constructed from env vars")
	}
	if !cfg.Telemetry.MetricsEnabled {
		t.Error("expected metrics_enabled = true")
	}
}

func TestEnvVarOverrideTelemetryOTLPEndpoint(t *testing.T) {
	t.Setenv("BLOCKYARD_TELEMETRY_OTLP_ENDPOINT", "collector:4317")
	cfg := loadFromString(t, telemetryTOML(t))
	if cfg.Telemetry.OTLPEndpoint != "collector:4317" {
		t.Errorf("otlp_endpoint = %q, want collector:4317", cfg.Telemetry.OTLPEndpoint)
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"trace", LevelTrace},
		{"TRACE", LevelTrace},
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
		{"  debug  ", slog.LevelDebug},
	}
	for _, tt := range tests {
		got := ParseLogLevel(tt.input)
		if got != tt.want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
