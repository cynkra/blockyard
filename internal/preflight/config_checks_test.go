package preflight

import (
	"testing"

	"github.com/cynkra/blockyard/internal/config"
)

func TestCheckNoOIDC(t *testing.T) {
	t.Run("fires when OIDC absent", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{Bind: "127.0.0.1:8080"},
		}
		r := RunConfigChecks(cfg)
		if !hasFinding(r, "no_oidc") {
			t.Error("expected no_oidc finding")
		}
	})

	t.Run("ok when OIDC configured", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{Bind: "127.0.0.1:8080"},
			OIDC:   &config.OidcConfig{},
		}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "no_oidc")
	})
}

func TestCheckWildcardBindNoOIDC(t *testing.T) {
	t.Run("fires on 0.0.0.0 without OIDC", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{Bind: "0.0.0.0:8080"},
		}
		r := RunConfigChecks(cfg)
		if !hasFinding(r, "wildcard_bind_no_oidc") {
			t.Error("expected wildcard_bind_no_oidc finding")
		}
	})

	t.Run("subsumes no_oidc", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{Bind: "0.0.0.0:8080"},
		}
		r := RunConfigChecks(cfg)
		if hasFinding(r, "no_oidc") {
			t.Error("no_oidc should be suppressed when wildcard_bind_no_oidc fires")
		}
	})

	t.Run("ok on loopback", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{Bind: "127.0.0.1:8080"},
		}
		r := RunConfigChecks(cfg)
		if hasFinding(r, "wildcard_bind_no_oidc") {
			t.Error("unexpected wildcard_bind_no_oidc finding on loopback")
		}
	})

	t.Run("ok with OIDC", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{Bind: "0.0.0.0:8080"},
			OIDC:   &config.OidcConfig{},
		}
		r := RunConfigChecks(cfg)
		if hasFinding(r, "wildcard_bind_no_oidc") {
			t.Error("unexpected wildcard_bind_no_oidc finding when OIDC configured")
		}
	})
}

func TestCheckExternalURLNotHTTPS(t *testing.T) {
	t.Run("fires on HTTP", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{ExternalURL: "http://example.com"},
		}
		r := RunConfigChecks(cfg)
		if !hasFinding(r, "external_url_not_https") {
			t.Error("expected external_url_not_https finding")
		}
	})

	t.Run("ok on HTTPS", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{ExternalURL: "https://example.com"},
		}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "external_url_not_https")
	})

	t.Run("ok when empty", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "external_url_not_https")
	})
}

func TestCheckVaultHTTP(t *testing.T) {
	t.Run("fires on HTTP", func(t *testing.T) {
		cfg := &config.Config{
			Vault: &config.VaultConfig{Address: "http://vault:8200"},
		}
		r := RunConfigChecks(cfg)
		if !hasFinding(r, "vault_http") {
			t.Error("expected vault_http finding")
		}
	})

	t.Run("ok on HTTPS", func(t *testing.T) {
		cfg := &config.Config{
			Vault: &config.VaultConfig{Address: "https://vault:8200"},
		}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "vault_http")
	})

	t.Run("ok when not configured", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "vault_http")
	})
}

func TestCheckManagementBindPublic(t *testing.T) {
	t.Run("fires on wildcard", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{ManagementBind: "0.0.0.0:9090"},
		}
		r := RunConfigChecks(cfg)
		if !hasFinding(r, "management_bind_public") {
			t.Error("expected management_bind_public finding")
		}
	})

	t.Run("ok on loopback", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{ManagementBind: "127.0.0.1:9090"},
		}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "management_bind_public")
	})

	t.Run("ok when not configured", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "management_bind_public")
	})
}

func TestCheckNoDefaultMemoryLimit(t *testing.T) {
	t.Run("fires when empty", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		if !hasFinding(r, "no_default_memory_limit") {
			t.Error("expected no_default_memory_limit finding")
		}
	})

	t.Run("ok when set", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{DefaultMemoryLimit: "2g"},
		}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "no_default_memory_limit")
	})
}

func TestCheckNoDefaultCPULimit(t *testing.T) {
	t.Run("fires when zero", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		if !hasFinding(r, "no_default_cpu_limit") {
			t.Error("expected no_default_cpu_limit finding")
		}
	})

	t.Run("ok when set", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{DefaultCPULimit: 4.0},
		}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "no_default_cpu_limit")
	})
}

func TestCheckNoAuditLog(t *testing.T) {
	t.Run("fires when absent", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		res := findResult(r, "no_audit_log")
		if res == nil {
			t.Fatal("expected no_audit_log result")
		}
		if res.Severity != SeverityInfo {
			t.Errorf("expected SeverityInfo, got %v", res.Severity)
		}
	})

	t.Run("ok when configured", func(t *testing.T) {
		cfg := &config.Config{
			Audit: &config.AuditConfig{Path: "/var/log/audit.jsonl"},
		}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "no_audit_log")
	})
}

func TestCheckTrustedProxiesTooBroad(t *testing.T) {
	t.Run("fires on /0", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{
				TrustedProxies: []string{"0.0.0.0/0"},
			},
		}
		r := RunConfigChecks(cfg)
		if !hasFinding(r, "trusted_proxies_too_broad") {
			t.Error("expected trusted_proxies_too_broad finding")
		}
	})

	t.Run("fires on /4", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{
				TrustedProxies: []string{"10.0.0.0/4"},
			},
		}
		r := RunConfigChecks(cfg)
		if !hasFinding(r, "trusted_proxies_too_broad") {
			t.Error("expected trusted_proxies_too_broad for /4")
		}
	})

	t.Run("ok on /8", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{
				TrustedProxies: []string{"10.0.0.0/8"},
			},
		}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "trusted_proxies_too_broad")
	})

	t.Run("ok when empty", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "trusted_proxies_too_broad")
	})

	t.Run("fires on IPv6 /8", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{
				TrustedProxies: []string{"2001:db8::/8"},
			},
		}
		r := RunConfigChecks(cfg)
		if !hasFinding(r, "trusted_proxies_too_broad") {
			t.Error("expected trusted_proxies_too_broad for IPv6 /8")
		}
	})

	t.Run("ok on IPv6 /16", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{
				TrustedProxies: []string{"2001:db8::/16"},
			},
		}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "trusted_proxies_too_broad")
	})

	t.Run("skips invalid CIDR", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{
				TrustedProxies: []string{"not-a-cidr"},
			},
		}
		r := RunConfigChecks(cfg)
		assertOK(t, r, "trusted_proxies_too_broad")
	})

	t.Run("multiple CIDRs first broad", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{
				TrustedProxies: []string{"10.0.0.0/24", "0.0.0.0/0"},
			},
		}
		r := RunConfigChecks(cfg)
		if !hasFinding(r, "trusted_proxies_too_broad") {
			t.Error("expected finding when any CIDR is too broad")
		}
	})
}

func TestIsWildcardBind(t *testing.T) {
	tests := []struct {
		bind string
		want bool
	}{
		{"0.0.0.0:8080", true},
		{"[::]:8080", true},
		{":8080", true},
		{"127.0.0.1:8080", false},
		{"10.0.0.1:8080", false},
		{"0.0.0.0", true},
		{"::", true},
		{"", true},
	}
	for _, tt := range tests {
		if got := isWildcardBind(tt.bind); got != tt.want {
			t.Errorf("isWildcardBind(%q) = %v, want %v", tt.bind, got, tt.want)
		}
	}
}

func TestResultCategory(t *testing.T) {
	cfg := &config.Config{}
	r := RunConfigChecks(cfg)
	for _, res := range r.Results {
		if res.Category != "config" {
			t.Errorf("result %q has category %q, want %q", res.Name, res.Category, "config")
		}
	}
}

// --- helpers ---

// hasFinding returns true if the report contains a non-OK result with the given name.
func hasFinding(r *Report, name string) bool {
	res := findResult(r, name)
	return res != nil && res.Severity > SeverityOK
}

// assertOK checks that the named result exists and has SeverityOK.
func assertOK(t *testing.T, r *Report, name string) {
	t.Helper()
	res := findResult(r, name)
	if res == nil {
		t.Errorf("expected result %q", name)
		return
	}
	if res.Severity != SeverityOK {
		t.Errorf("result %q: expected SeverityOK, got %v", name, res.Severity)
	}
}

func findResult(r *Report, name string) *Result {
	for i := range r.Results {
		if r.Results[i].Name == name {
			return &r.Results[i]
		}
	}
	return nil
}
