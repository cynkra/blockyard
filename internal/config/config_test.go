package config

import (
	"bytes"
	"encoding/json"
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

// captureSlog routes slog.Default output into a buffer for the lifetime
// of the test, so assertions can inspect warning messages. Restores the
// previous default logger on cleanup.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestParseMinimalConfig(t *testing.T) {
	cfg := loadFromString(t, minimalTOML)
	if cfg.Server.Bind != "127.0.0.1:8080" {
		t.Errorf("expected default bind, got %q", cfg.Server.Bind)
	}
	if cfg.Proxy.MaxWorkers != 100 {
		t.Errorf("expected default max_workers 100, got %d", cfg.Proxy.MaxWorkers)
	}
}

func TestDefaultShutdownAndDrainTimeouts(t *testing.T) {
	cfg := loadFromString(t, minimalTOML)
	if cfg.Server.ShutdownTimeout.Duration != 30*time.Second {
		t.Errorf("expected ShutdownTimeout 30s, got %v", cfg.Server.ShutdownTimeout.Duration)
	}
	if cfg.Server.DrainTimeout.Duration != 30*time.Second {
		t.Errorf("expected DrainTimeout 30s, got %v", cfg.Server.DrainTimeout.Duration)
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

func TestSecretMarshalJSON(t *testing.T) {
	s := NewSecret("super-secret")
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"[REDACTED]"` {
		t.Errorf("MarshalJSON() = %s, want %q", data, "[REDACTED]")
	}
}

func TestSecretMarshalText(t *testing.T) {
	s := NewSecret("super-secret")
	data, err := s.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[REDACTED]" {
		t.Errorf("MarshalText() = %q, want [REDACTED]", data)
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
	if cfg.OIDC.DefaultRole != "viewer" {
		t.Errorf("expected default_role viewer, got %q", cfg.OIDC.DefaultRole)
	}
}

func TestOidcDefaultRolePublisher(t *testing.T) {
	toml := oidcTOML(t) + "default_role = \"publisher\"\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected publisher to be accepted, got: %v", err)
	}
	if cfg.OIDC.DefaultRole != "publisher" {
		t.Errorf("default_role = %q, want publisher", cfg.OIDC.DefaultRole)
	}
}

func TestOidcDefaultRoleRejectsAdmin(t *testing.T) {
	toml := oidcTOML(t) + "default_role = \"admin\"\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "oidc.default_role") {
		t.Errorf("expected oidc.default_role error, got: %v", err)
	}
}

func TestOidcDefaultRoleRejectsUnknown(t *testing.T) {
	toml := oidcTOML(t) + "default_role = \"editor\"\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "oidc.default_role") {
		t.Errorf("expected oidc.default_role error, got: %v", err)
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
	// Without [vault], session_secret is required.
	toml := strings.Replace(oidcTOML(t), `session_secret = "my-session-secret"`, ``, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "session_secret") {
		t.Errorf("expected session_secret error, got: %v", err)
	}
}

func TestValidationDefersSessionSecretWithVault(t *testing.T) {
	// With [vault] configured, session_secret is not required at load time
	// (it may be auto-generated or resolved from vault).
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	toml := fmt.Sprintf(`
[server]
external_url = "https://example.com"

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

[vault]
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
external_url = "https://example.com"

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

[vault]
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

func TestParseVaultRoleID(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	toml := fmt.Sprintf(`
[server]
session_secret = "my-session-secret"
external_url = "https://example.com"

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

[vault]
address = "https://bao.example.com"
role_id = "blockyard-server"
`, bundlePath, dbPath)
	cfg := loadFromString(t, toml)
	if cfg.Vault == nil {
		t.Fatal("expected Vault config")
	}
	if cfg.Vault.RoleID != "blockyard-server" {
		t.Errorf("role_id = %q, want blockyard-server", cfg.Vault.RoleID)
	}
	if !cfg.Vault.AdminToken.IsEmpty() {
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

	t.Setenv("BLOCKYARD_SERVER_EXTERNAL_URL", "https://example.com")
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

func vaultTOML(t *testing.T) string {
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

[vault]
address = "https://bao.example.com"
admin_token = "hvs.admin123"
`, bundlePath, dbPath)
}

func TestParseVaultConfig(t *testing.T) {
	cfg := loadFromString(t, vaultTOML(t))
	if cfg.Vault == nil {
		t.Fatal("expected Vault config to be parsed")
	}
	if cfg.Vault.Address != "https://bao.example.com" {
		t.Errorf("address = %q", cfg.Vault.Address)
	}
	if cfg.Vault.AdminToken.MustExpose() != "hvs.admin123" {
		t.Errorf("admin_token = %q", cfg.Vault.AdminToken.MustExpose())
	}
	if cfg.Vault.TokenTTL.Duration != 1*time.Hour {
		t.Errorf("expected default token_ttl 1h, got %v", cfg.Vault.TokenTTL.Duration)
	}
	if cfg.Vault.JWTAuthPath != "jwt" {
		t.Errorf("expected default jwt_auth_path 'jwt', got %q", cfg.Vault.JWTAuthPath)
	}
	if cfg.Vault.SecretIDFile != "" {
		t.Errorf("expected secret_id_file empty by default, got %q", cfg.Vault.SecretIDFile)
	}
}

func TestVaultSecretIDFileSet(t *testing.T) {
	toml := strings.Replace(
		vaultTOML(t),
		`admin_token = "hvs.admin123"`,
		`admin_token    = "hvs.admin123"`+"\n"+`secret_id_file = "/run/secrets/vault_secret_id"`,
		1,
	)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Vault.SecretIDFile != "/run/secrets/vault_secret_id" {
		t.Errorf("secret_id_file = %q, want /run/secrets/vault_secret_id", cfg.Vault.SecretIDFile)
	}
}

func TestParseConfigWithoutVault(t *testing.T) {
	cfg := loadFromString(t, minimalTOML)
	if cfg.Vault != nil {
		t.Error("expected Vault config to be nil when section is absent")
	}
}

func TestValidationRejectsVaultEmptyAddress(t *testing.T) {
	toml := strings.Replace(vaultTOML(t), `address = "https://bao.example.com"`, `address = ""`, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "vault.address") {
		t.Errorf("expected vault.address error, got: %v", err)
	}
}

func TestValidationRejectsVaultNoCredentials(t *testing.T) {
	toml := strings.Replace(vaultTOML(t), `admin_token = "hvs.admin123"`, `admin_token = ""`, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "requires either admin_token or role_id") {
		t.Errorf("expected 'requires either admin_token or role_id' error, got: %v", err)
	}
}

func TestValidationRejectsVaultWithoutOidc(t *testing.T) {
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

[vault]
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

func TestVaultAutoConstructFromEnvVars(t *testing.T) {
	t.Setenv("BLOCKYARD_VAULT_ADDRESS", "https://env-bao.example.com")
	t.Setenv("BLOCKYARD_VAULT_ADMIN_TOKEN", "hvs.env-token")
	// Also need OIDC for vault validation to pass.
	t.Setenv("BLOCKYARD_SERVER_EXTERNAL_URL", "https://example.com")
	t.Setenv("BLOCKYARD_OIDC_ISSUER_URL", "https://idp.example.com")
	t.Setenv("BLOCKYARD_OIDC_CLIENT_ID", "my-client")
	t.Setenv("BLOCKYARD_OIDC_CLIENT_SECRET", "my-secret")
	t.Setenv("BLOCKYARD_SERVER_SESSION_SECRET", "my-session-secret")

	cfg := loadFromString(t, minimalTOML)
	if cfg.Vault == nil {
		t.Fatal("expected Vault section to be auto-constructed from env vars")
	}
	if cfg.Vault.Address != "https://env-bao.example.com" {
		t.Errorf("address = %q", cfg.Vault.Address)
	}
	if cfg.Vault.AdminToken.MustExpose() != "hvs.env-token" {
		t.Errorf("admin_token = %q", cfg.Vault.AdminToken.MustExpose())
	}
}

func TestEnvVarOverrideVaultTokenTTL(t *testing.T) {
	t.Setenv("BLOCKYARD_VAULT_TOKEN_TTL", "30m")
	cfg := loadFromString(t, vaultTOML(t))
	if cfg.Vault.TokenTTL.Duration != 30*time.Minute {
		t.Errorf("token_ttl = %v, want 30m", cfg.Vault.TokenTTL.Duration)
	}
}

// --- Deprecation shim tests ---

// TestDeprecatedOpenbaoSectionMigrates verifies that a legacy [openbao]
// section still loads: it gets moved into cfg.Vault at load time.
func TestDeprecatedOpenbaoSectionMigrates(t *testing.T) {
	toml := strings.Replace(vaultTOML(t), "[vault]", "[openbao]", 1)
	cfg := loadFromString(t, toml)
	if cfg.Vault == nil {
		t.Fatal("expected [openbao] to migrate into cfg.Vault")
	}
	if cfg.Vault.Address != "https://bao.example.com" {
		t.Errorf("address = %q", cfg.Vault.Address)
	}
	if cfg.Openbao != nil {
		t.Error("expected cfg.Openbao to be cleared after migration")
	}
}

// TestDeprecatedOpenbaoEnvVarMigrates verifies that BLOCKYARD_OPENBAO_*
// env vars are translated to BLOCKYARD_VAULT_* before the override walk.
func TestDeprecatedOpenbaoEnvVarMigrates(t *testing.T) {
	t.Setenv("BLOCKYARD_OPENBAO_TOKEN_TTL", "45m")
	cfg := loadFromString(t, vaultTOML(t))
	if cfg.Vault.TokenTTL.Duration != 45*time.Minute {
		t.Errorf("token_ttl = %v, want 45m (from deprecated env var)", cfg.Vault.TokenTTL.Duration)
	}
}

// TestRemovedVaultTokenFileWarns verifies that an upgraded config still
// carrying the removed vault.token_file key triggers an explicit
// deprecation warning rather than being silently dropped by the TOML
// decoder.
func TestRemovedVaultTokenFileWarns(t *testing.T) {
	logs := captureSlog(t)
	toml := vaultTOML(t) + "token_file = \"/data/.vault-token\"\n"
	loadFromString(t, toml)
	if !strings.Contains(logs.String(), "vault.token_file is deprecated") {
		t.Errorf("expected deprecation warning for vault.token_file, got logs:\n%s", logs.String())
	}
}

// TestRemovedVaultTokenFileNotPresent ensures the deprecation warning
// does not fire when the key is absent — otherwise every startup noise
// would be indistinguishable from an actual upgrade problem.
func TestRemovedVaultTokenFileNotPresent(t *testing.T) {
	logs := captureSlog(t)
	loadFromString(t, vaultTOML(t))
	if strings.Contains(logs.String(), "vault.token_file is deprecated") {
		t.Errorf("unexpected token_file warning for clean config:\n%s", logs.String())
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

// --- Database vault / board-storage config tests ---

// databaseVaultTOML returns a config with the database section populated
// per overrides. `overrides` is appended verbatim to [database]; other
// sections (docker, storage, oidc, vault) follow the happy-path shape
// so validation fails only on the specific case under test.
func databaseVaultTOML(t *testing.T, overrides string) string {
	t.Helper()
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	return fmt.Sprintf(`
[server]
external_url = "https://example.com"

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
driver = "postgres"
url = "postgres://blockyard:blockyard@localhost:5432/blockyard?sslmode=disable"
%s

[oidc]
issuer_url = "https://idp.example.com"
client_id = "my-client"
client_secret = "my-secret"

[vault]
address = "https://bao.example.com"
role_id = "blockyard-server"
`, bundlePath, overrides)
}

func TestDatabaseVaultMountDefault(t *testing.T) {
	cfg := loadFromString(t, minimalTOML)
	if cfg.Database.VaultMount != "database" {
		t.Errorf("vault_mount default = %q, want %q", cfg.Database.VaultMount, "database")
	}
}

func TestDatabaseBoardStorageParsed(t *testing.T) {
	cfg := loadFromString(t, databaseVaultTOML(t,
		`board_storage = true`+"\n"+
			`vault_db_connection = "postgresql"`))
	if !cfg.Database.BoardStorage {
		t.Error("expected database.board_storage = true")
	}
	if cfg.Database.VaultMount != "database" {
		t.Errorf("vault_mount = %q, want default %q", cfg.Database.VaultMount, "database")
	}
	if cfg.Database.VaultDBConnection != "postgresql" {
		t.Errorf("vault_db_connection = %q", cfg.Database.VaultDBConnection)
	}
	if cfg.Database.VaultRotationPeriod.Duration != 24*time.Hour {
		t.Errorf("vault_rotation_period default = %v, want 24h", cfg.Database.VaultRotationPeriod.Duration)
	}
}

func TestDatabaseVaultRotationPeriodParsed(t *testing.T) {
	cfg := loadFromString(t, databaseVaultTOML(t,
		`board_storage = true`+"\n"+
			`vault_db_connection = "postgresql"`+"\n"+
			`vault_rotation_period = "72h"`))
	if cfg.Database.VaultRotationPeriod.Duration != 72*time.Hour {
		t.Errorf("vault_rotation_period = %v, want 72h", cfg.Database.VaultRotationPeriod.Duration)
	}
}

func TestValidationRejectsBoardStorageWithoutVaultDBConnection(t *testing.T) {
	expectLoadError(t,
		databaseVaultTOML(t, `board_storage = true`),
		"database.board_storage requires database.vault_db_connection")
}

func TestDatabaseVaultRoleParsed(t *testing.T) {
	cfg := loadFromString(t, databaseVaultTOML(t,
		`vault_mount = "dbx"`+"\n"+
			`vault_role = "blockyard_app"`))
	if cfg.Database.VaultRole != "blockyard_app" {
		t.Errorf("vault_role = %q", cfg.Database.VaultRole)
	}
	if cfg.Database.VaultMount != "dbx" {
		t.Errorf("vault_mount = %q", cfg.Database.VaultMount)
	}
}

// expectLoadError writes toml to a temp file, calls Load, and asserts
// the returned error contains substr. Keeps the six validation cases
// below to one-liners.
func expectLoadError(t *testing.T, toml, substr string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Errorf("expected error containing %q, got: %v", substr, err)
	}
}

// noVaultTOML is databaseVaultTOML without the [vault] section.
// session_secret is set explicitly because the "oidc without vault"
// validation fires before the database-specific checks we want to hit.
func noVaultTOML(t *testing.T, overrides string) string {
	t.Helper()
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	return fmt.Sprintf(`
[server]
external_url   = "https://example.com"
session_secret = "test-session-secret"

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
driver = "postgres"
url    = "postgres://blockyard:blockyard@localhost:5432/blockyard?sslmode=disable"
%s

[oidc]
issuer_url    = "https://idp.example.com"
client_id     = "my-client"
client_secret = "my-secret"
`, bundlePath, overrides)
}

// sqliteVaultTOML is a non-postgres config with the overrides injected
// under [database]. [vault] is present so the check under test fires
// on driver, not on vault.
func sqliteVaultTOML(t *testing.T, overrides string) string {
	t.Helper()
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundles")
	dbPath := filepath.Join(dir, "db", "blockyard.db")
	return fmt.Sprintf(`
[server]
external_url = "https://example.com"

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = %q

[database]
path = %q
%s

[oidc]
issuer_url = "https://idp.example.com"
client_id = "my-client"
client_secret = "my-secret"

[vault]
address = "https://bao.example.com"
role_id = "blockyard-server"
`, bundlePath, dbPath, overrides)
}

func TestValidationRejectsVaultRoleWithoutVault(t *testing.T) {
	expectLoadError(t,
		noVaultTOML(t, `vault_role = "blockyard_app"`),
		"database.vault_role requires [vault]")
}

func TestValidationRejectsVaultRoleWithoutPostgres(t *testing.T) {
	expectLoadError(t,
		sqliteVaultTOML(t, `vault_role = "blockyard_app"`),
		`database.vault_role requires database.driver = "postgres"`)
}

func TestValidationRejectsBoardStorageWithoutVault(t *testing.T) {
	expectLoadError(t,
		noVaultTOML(t, `board_storage = true`),
		"database.board_storage requires [vault]")
}

func TestValidationRejectsBoardStorageWithoutPostgres(t *testing.T) {
	expectLoadError(t,
		sqliteVaultTOML(t, `board_storage = true`),
		`database.board_storage requires database.driver = "postgres"`)
}

// validDatabaseConfigForVault returns a Config that passes validate()
// except for whatever the caller modifies afterwards — specifically
// used to exercise validation paths the Load() pipeline cannot reach
// because applyDefaults overwrites an empty vault_mount.
func validDatabaseConfigForVault(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	return &Config{
		Server: ServerConfig{
			Backend:       "docker",
			Bind:          ":8080",
			ExternalURL:   "https://example.com",
			SessionSecret: secretPtr("test"),
		},
		Docker:  DockerConfig{Image: "ghcr.io/rocker-org/r-ver:latest"},
		Storage: StorageConfig{BundleServerPath: filepath.Join(dir, "bundles")},
		Database: DatabaseConfig{
			Driver:     "postgres",
			URL:        "postgres://u:p@h/d",
			VaultMount: "database",
		},
		OIDC: &OidcConfig{
			IssuerURL:    "https://idp.example.com",
			ClientID:     "c",
			ClientSecret: NewSecret("s"),
			DefaultRole:  "viewer",
		},
		Vault: &VaultConfig{
			Address: "https://bao.example.com",
			RoleID:  "blockyard-server",
		},
	}
}

func secretPtr(s string) *Secret { v := NewSecret(s); return &v }

// TestValidateEmptyVaultMount exercises the two empty-vault_mount cases
// directly against validate() because applyDefaults always rewrites an
// empty vault_mount to "database" when going through Load().
func TestValidateEmptyVaultMount(t *testing.T) {
	t.Run("vault_role", func(t *testing.T) {
		cfg := validDatabaseConfigForVault(t)
		cfg.Database.VaultRole = "blockyard_app"
		cfg.Database.VaultMount = ""
		err := validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "database.vault_role requires database.vault_mount") {
			t.Errorf("want vault_role-requires-vault_mount, got: %v", err)
		}
	})
	t.Run("board_storage", func(t *testing.T) {
		cfg := validDatabaseConfigForVault(t)
		cfg.Database.BoardStorage = true
		cfg.Database.VaultMount = ""
		err := validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "database.board_storage requires database.vault_mount") {
			t.Errorf("want board_storage-requires-vault_mount, got: %v", err)
		}
	})
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

func TestEnvVarOverrideTrustedProxies(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "blockyard.toml")
	os.WriteFile(cfgPath, []byte(minimalTOML), 0644)
	os.MkdirAll("/tmp/blockyard-test/bundles", 0755)
	os.MkdirAll("/tmp/blockyard-test/db", 0755)

	t.Setenv("BLOCKYARD_SERVER_TRUSTED_PROXIES", "10.0.0.0/8, 172.16.0.0/12")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Server.TrustedProxies) != 2 {
		t.Fatalf("expected 2 trusted proxies, got %d: %v", len(cfg.Server.TrustedProxies), cfg.Server.TrustedProxies)
	}
	if cfg.Server.TrustedProxies[0] != "10.0.0.0/8" {
		t.Errorf("expected first proxy=10.0.0.0/8, got %s", cfg.Server.TrustedProxies[0])
	}
}

func TestEnvVarOverrideBundleRetention(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "blockyard.toml")
	os.WriteFile(cfgPath, []byte(minimalTOML), 0644)
	os.MkdirAll("/tmp/blockyard-test/bundles", 0755)
	os.MkdirAll("/tmp/blockyard-test/db", 0755)

	t.Setenv("BLOCKYARD_STORAGE_BUNDLE_RETENTION", "99")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.BundleRetention != 99 {
		t.Errorf("expected BundleRetention=99, got %d", cfg.Storage.BundleRetention)
	}
}

func TestValidationRejectsInvalidTrustedProxyCIDR(t *testing.T) {
	tmp := t.TempDir()
	toml := `
[server]
trusted_proxies = ["not-a-cidr"]

[docker]
image = "test"

[storage]
bundle_server_path = "` + filepath.ToSlash(filepath.Join(tmp, "bundles")) + `"

[database]
path = "` + filepath.ToSlash(filepath.Join(tmp, "db", "test.db")) + `"

[proxy]
`
	os.MkdirAll(filepath.Join(tmp, "bundles"), 0755)
	os.MkdirAll(filepath.Join(tmp, "db"), 0755)
	cfgPath := filepath.Join(tmp, "config.toml")
	os.WriteFile(cfgPath, []byte(toml), 0644)

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("expected validation error for invalid CIDR")
	}
	if !strings.Contains(err.Error(), "CIDR") {
		t.Errorf("expected CIDR error, got: %v", err)
	}
}

func TestValidationRejectsOidcWithoutExternalURL(t *testing.T) {
	toml := strings.Replace(oidcTOML(t),
		`external_url = "https://example.com"`,
		``, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(toml), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "external_url") {
		t.Errorf("expected external_url error, got: %v", err)
	}
}

func TestUpdateDefaults(t *testing.T) {
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

[update]
`
	cfg := loadFromString(t, tomlContent)
	if cfg.Update == nil {
		t.Fatal("expected Update section to be non-nil")
	}
	if cfg.Update.Channel != "stable" {
		t.Errorf("default channel = %q, want %q", cfg.Update.Channel, "stable")
	}
	if cfg.Update.WatchPeriod.Duration != 5*time.Minute {
		t.Errorf("default watch_period = %v, want 5m", cfg.Update.WatchPeriod.Duration)
	}
}

func TestRedisDefaults(t *testing.T) {
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

[redis]
url = "redis://localhost:6379"
`
	cfg := loadFromString(t, tomlContent)
	if cfg.Redis == nil {
		t.Fatal("expected Redis section to be non-nil")
	}
	if cfg.Redis.KeyPrefix != "blockyard:" {
		t.Errorf("default key_prefix = %q, want %q", cfg.Redis.KeyPrefix, "blockyard:")
	}
}

func TestRedisDefaultsAppendColon(t *testing.T) {
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

[redis]
url = "redis://localhost:6379"
key_prefix = "myprefix"
`
	cfg := loadFromString(t, tomlContent)
	if cfg.Redis.KeyPrefix != "myprefix:" {
		t.Errorf("key_prefix = %q, want %q (colon appended)", cfg.Redis.KeyPrefix, "myprefix:")
	}
}

func TestEnvVarCreatesRedisSection(t *testing.T) {
	t.Setenv("BLOCKYARD_REDIS_URL", "redis://localhost:6379")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Redis == nil {
		t.Fatal("expected Redis section created from env var")
	}
	if cfg.Redis.URL != "redis://localhost:6379" {
		t.Errorf("Redis.URL = %q", cfg.Redis.URL)
	}
}

func TestEnvVarCreatesUpdateSection(t *testing.T) {
	t.Setenv("BLOCKYARD_UPDATE_CHANNEL", "main")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Update == nil {
		t.Fatal("expected Update section created from env var")
	}
	if cfg.Update.Channel != "main" {
		t.Errorf("Update.Channel = %q, want %q", cfg.Update.Channel, "main")
	}
}

func TestLogLevelParsing(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
	}
	for _, tt := range tests {
		if got := ParseLogLevel(tt.input); got != tt.want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestValidate_DataMountDuplicateNames(t *testing.T) {
	err := validateDataMounts([]DataMountSource{
		{Name: "models", Path: "/data/models"},
		{Name: "models", Path: "/data/models2"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got: %v", err)
	}
}

func TestValidate_DataMountRelativePath(t *testing.T) {
	err := validateDataMounts([]DataMountSource{
		{Name: "models", Path: "relative"},
	})
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Errorf("expected absolute path error, got: %v", err)
	}
}

func TestValidate_DataMountEmptyName(t *testing.T) {
	err := validateDataMounts([]DataMountSource{
		{Name: "", Path: "/data/models"},
	})
	if err == nil || !strings.Contains(err.Error(), "name must not be empty") {
		t.Errorf("expected empty name error, got: %v", err)
	}
}

func TestValidate_DataMountInvalidChars(t *testing.T) {
	err := validateDataMounts([]DataMountSource{
		{Name: "models/v2", Path: "/data/models"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid characters") {
		t.Errorf("expected invalid characters error, got: %v", err)
	}
}

func TestValidate_SessionStoreUnknownValue(t *testing.T) {
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

[proxy]
session_store = "bogus"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(tomlContent), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "session_store") {
		t.Errorf("expected session_store validation error, got: %v", err)
	}
}

func TestValidate_SessionStoreLayeredRequiresPostgres(t *testing.T) {
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

[redis]
url = "redis://localhost:6379"

[proxy]
session_store = "layered"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(tomlContent), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Errorf("expected postgres requirement error, got: %v", err)
	}
}

func TestValidate_SessionStoreRedisRequiresRedisSection(t *testing.T) {
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

[proxy]
session_store = "redis"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(tomlContent), 0o644)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "[redis]") {
		t.Errorf("expected redis requirement error, got: %v", err)
	}
}

func TestValidate_DataMountValid(t *testing.T) {
	err := validateDataMounts([]DataMountSource{
		{Name: "models", Path: "/data/models"},
		{Name: "scratch-2", Path: "/data/scratch"},
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_RuntimeDefaultsValid(t *testing.T) {
	err := validateRuntimeDefaults(map[string]string{
		"public": "kata-runtime",
		"acl":    "runc",
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_RuntimeDefaultsInvalidKey(t *testing.T) {
	err := validateRuntimeDefaults(map[string]string{
		"unknown": "runc",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown access type") {
		t.Errorf("expected unknown access type error, got: %v", err)
	}
}

func TestValidate_RuntimeDefaultsEmpty(t *testing.T) {
	err := validateRuntimeDefaults(nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestProcessBackendConfig(t *testing.T) {
	const processTOML = `
[server]
backend = "process"

[process]
bwrap_path             = "/usr/bin/bwrap"
r_path                 = "/usr/bin/R"
port_range_start       = 11000
port_range_end         = 11099
worker_uid_range_start = 65000
worker_uid_range_end   = 65099
worker_gid             = 65534

[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"

[proxy]
`
	cfg := loadFromString(t, processTOML)
	if cfg.Server.Backend != "process" {
		t.Errorf("expected backend=process, got %q", cfg.Server.Backend)
	}
	if cfg.Process == nil {
		t.Fatal("expected [process] config to be set")
	}
	if cfg.Process.PortRangeStart != 11000 {
		t.Errorf("expected port range start 11000, got %d", cfg.Process.PortRangeStart)
	}
	if cfg.Process.WorkerGID != 65534 {
		t.Errorf("expected worker GID 65534, got %d", cfg.Process.WorkerGID)
	}
}

func TestProcessBackendDefaults(t *testing.T) {
	const processTOML = `
[server]
backend = "process"

[process]

[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"

[proxy]
`
	cfg := loadFromString(t, processTOML)
	if cfg.Process.BwrapPath != "/usr/bin/bwrap" {
		t.Errorf("default bwrap_path = %q", cfg.Process.BwrapPath)
	}
	if cfg.Process.PortRangeStart != 10000 || cfg.Process.PortRangeEnd != 10999 {
		t.Errorf("default port range = %d..%d", cfg.Process.PortRangeStart, cfg.Process.PortRangeEnd)
	}
	if cfg.Process.WorkerUIDStart != 60000 || cfg.Process.WorkerUIDEnd != 60999 {
		t.Errorf("default uid range = %d..%d", cfg.Process.WorkerUIDStart, cfg.Process.WorkerUIDEnd)
	}
	if cfg.Process.WorkerGID != 65534 {
		t.Errorf("default worker_gid = %d", cfg.Process.WorkerGID)
	}
}

func TestProcessBackendRequiresProcessSection(t *testing.T) {
	const badTOML = `
[server]
backend = "process"

[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"

[proxy]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	if err := os.WriteFile(path, []byte(badTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error when [process] section is missing")
	}
}

func TestProcessBackendUIDRangeMustCoverPortRange(t *testing.T) {
	const badTOML = `
[server]
backend = "process"

[process]
port_range_start       = 10000
port_range_end         = 10099
worker_uid_range_start = 60000
worker_uid_range_end   = 60010

[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"

[proxy]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	if err := os.WriteFile(path, []byte(badTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error when uid range smaller than port range")
	}
}

func TestUnknownBackendRejected(t *testing.T) {
	const badTOML = `
[server]
backend = "k8s"

[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"

[proxy]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	if err := os.WriteFile(path, []byte(badTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestDeprecatedFieldMigration(t *testing.T) {
	const oldTOML = `
[server]
skip_docker_preflight = true

[docker]
image                = "img"
default_memory_limit = "2g"
default_cpu_limit    = 4.0
store_retention      = "24h"

[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"

[proxy]
`
	cfg := loadFromString(t, oldTOML)
	if cfg.Server.DefaultMemoryLimit != "2g" {
		t.Errorf("default_memory_limit not migrated: got %q", cfg.Server.DefaultMemoryLimit)
	}
	if cfg.Server.DefaultCPULimit != 4.0 {
		t.Errorf("default_cpu_limit not migrated: got %v", cfg.Server.DefaultCPULimit)
	}
	if cfg.Storage.StoreRetention.Duration != 24*time.Hour {
		t.Errorf("store_retention not migrated: got %v", cfg.Storage.StoreRetention.Duration)
	}
	if !cfg.Server.SkipPreflight {
		t.Errorf("skip_docker_preflight not migrated")
	}
}

func TestNewFieldWinsOverDeprecated(t *testing.T) {
	const bothTOML = `
[server]
default_memory_limit = "8g"

[docker]
image                = "img"
default_memory_limit = "2g"

[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"

[proxy]
`
	cfg := loadFromString(t, bothTOML)
	if cfg.Server.DefaultMemoryLimit != "8g" {
		t.Errorf("expected new field to win, got %q", cfg.Server.DefaultMemoryLimit)
	}
}

// TestProcessBackendReversedRanges — end<start on either range is a
// common typo; validate must surface the offending field before
// downstream math goes negative.
func TestProcessBackendReversedRanges(t *testing.T) {
	cases := []struct {
		name         string
		processBlock string
		wantField    string
	}{
		{
			name: "port_range",
			processBlock: `
port_range_start       = 11000
port_range_end         = 10999
worker_uid_range_start = 60000
worker_uid_range_end   = 60999
`,
			wantField: "port_range_end",
		},
		{
			name: "worker_uid_range",
			processBlock: `
port_range_start       = 10000
port_range_end         = 10099
worker_uid_range_start = 60100
worker_uid_range_end   = 60000
`,
			wantField: "worker_uid_range_end",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			badTOML := `
[server]
backend = "process"

[process]` + tc.processBlock + `
[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"

[proxy]
`
			dir := t.TempDir()
			path := filepath.Join(dir, "blockyard.toml")
			if err := os.WriteFile(path, []byte(badTOML), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error on reversed %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error should name %q: %v", tc.wantField, err)
			}
		})
	}
}
