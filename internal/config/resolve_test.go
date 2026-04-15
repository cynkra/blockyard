package config

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// mockResolver implements SecretResolver for tests.
type mockResolver struct {
	secrets map[string]map[string]any // path → data
}

func (m *mockResolver) KVReadAdmin(_ context.Context, path string) (map[string]any, error) {
	data, ok := m.secrets[path]
	if !ok {
		return nil, fmt.Errorf("secret not found at %s", path)
	}
	return data, nil
}

func TestResolveSecretsResolvesOIDCClientSecret(t *testing.T) {
	cfg := &Config{
		OIDC: &OidcConfig{
			ClientSecret: NewSecret("vault:secret/data/blockyard/oidc#client_secret"),
		},
	}

	resolver := &mockResolver{
		secrets: map[string]map[string]any{
			"secret/data/blockyard/oidc": {"client_secret": "resolved-value"},
		},
	}

	if err := ResolveSecrets(context.Background(), cfg, resolver); err != nil {
		t.Fatal(err)
	}

	if cfg.OIDC.ClientSecret.MustExpose() != "resolved-value" {
		t.Errorf("client_secret = %q", cfg.OIDC.ClientSecret.MustExpose())
	}
}

func TestResolveSecretsResolvesSessionSecret(t *testing.T) {
	secret := NewSecret("vault:secret/data/blockyard/server#session_secret")
	cfg := &Config{
		Server: ServerConfig{
			SessionSecret: &secret,
		},
	}

	resolver := &mockResolver{
		secrets: map[string]map[string]any{
			"secret/data/blockyard/server": {"session_secret": "my-session-key"},
		},
	}

	if err := ResolveSecrets(context.Background(), cfg, resolver); err != nil {
		t.Fatal(err)
	}

	if cfg.Server.SessionSecret.MustExpose() != "my-session-key" {
		t.Errorf("session_secret = %q", cfg.Server.SessionSecret.MustExpose())
	}
}

func TestResolveSecretsLeavesLiteralsUnchanged(t *testing.T) {
	cfg := &Config{
		OIDC: &OidcConfig{
			ClientSecret: NewSecret("plain-secret"),
		},
	}

	resolver := &mockResolver{secrets: map[string]map[string]any{}}

	if err := ResolveSecrets(context.Background(), cfg, resolver); err != nil {
		t.Fatal(err)
	}

	if cfg.OIDC.ClientSecret.MustExpose() != "plain-secret" {
		t.Errorf("client_secret = %q", cfg.OIDC.ClientSecret.MustExpose())
	}
}

func TestResolveSecretsErrorOnMissingPath(t *testing.T) {
	cfg := &Config{
		OIDC: &OidcConfig{
			ClientSecret: NewSecret("vault:secret/data/missing#key"),
		},
	}

	resolver := &mockResolver{secrets: map[string]map[string]any{}}

	err := ResolveSecrets(context.Background(), cfg, resolver)
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "client_secret") {
		t.Errorf("error should mention field name: %v", err)
	}
}

func TestResolveSecretsErrorOnMissingKey(t *testing.T) {
	cfg := &Config{
		OIDC: &OidcConfig{
			ClientSecret: NewSecret("vault:secret/data/blockyard/oidc#wrong_key"),
		},
	}

	resolver := &mockResolver{
		secrets: map[string]map[string]any{
			"secret/data/blockyard/oidc": {"other_key": "value"},
		},
	}

	err := ResolveSecrets(context.Background(), cfg, resolver)
	if err == nil {
		t.Fatal("expected error for missing key")
	}
	if !strings.Contains(err.Error(), "wrong_key") {
		t.Errorf("error should mention key: %v", err)
	}
}

func TestResolveSecretsErrorOnInvalidFormat(t *testing.T) {
	cfg := &Config{
		OIDC: &OidcConfig{
			ClientSecret: NewSecret("vault:no-hash-separator"),
		},
	}

	resolver := &mockResolver{secrets: map[string]map[string]any{}}

	err := ResolveSecrets(context.Background(), cfg, resolver)
	if err == nil {
		t.Fatal("expected error for invalid format")
	}
	if !strings.Contains(err.Error(), "invalid vault reference") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveSecretsSkipsNilOpenbao(t *testing.T) {
	cfg := &Config{} // Openbao is nil
	resolver := &mockResolver{secrets: map[string]map[string]any{}}
	if err := ResolveSecrets(context.Background(), cfg, resolver); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSecretsOpenbaoAdminToken(t *testing.T) {
	cfg := &Config{
		Openbao: &OpenbaoConfig{
			AdminToken: NewSecret("vault:secret/data/blockyard/openbao#admin_token"),
		},
	}

	resolver := &mockResolver{
		secrets: map[string]map[string]any{
			"secret/data/blockyard/openbao": {"admin_token": "resolved-admin"},
		},
	}

	if err := ResolveSecrets(context.Background(), cfg, resolver); err != nil {
		t.Fatal(err)
	}

	if cfg.Openbao.AdminToken.MustExpose() != "resolved-admin" {
		t.Errorf("admin_token = %q", cfg.Openbao.AdminToken.MustExpose())
	}
}
