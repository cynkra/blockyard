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
		if !hasResult(r, "no_oidc") {
			t.Error("expected no_oidc result")
		}
	})

	t.Run("silent when OIDC configured", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{Bind: "127.0.0.1:8080"},
			OIDC:   &config.OidcConfig{},
		}
		r := RunConfigChecks(cfg)
		if hasResult(r, "no_oidc") {
			t.Error("unexpected no_oidc result")
		}
	})
}

func TestCheckWildcardBindNoOIDC(t *testing.T) {
	t.Run("fires on 0.0.0.0 without OIDC", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{Bind: "0.0.0.0:8080"},
		}
		r := RunConfigChecks(cfg)
		if !hasResult(r, "wildcard_bind_no_oidc") {
			t.Error("expected wildcard_bind_no_oidc result")
		}
	})

	t.Run("subsumes no_oidc", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{Bind: "0.0.0.0:8080"},
		}
		r := RunConfigChecks(cfg)
		if hasResult(r, "no_oidc") {
			t.Error("no_oidc should be suppressed when wildcard_bind_no_oidc fires")
		}
	})

	t.Run("silent on loopback", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{Bind: "127.0.0.1:8080"},
		}
		r := RunConfigChecks(cfg)
		if hasResult(r, "wildcard_bind_no_oidc") {
			t.Error("unexpected wildcard_bind_no_oidc on loopback")
		}
	})

	t.Run("silent with OIDC", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{Bind: "0.0.0.0:8080"},
			OIDC:   &config.OidcConfig{},
		}
		r := RunConfigChecks(cfg)
		if hasResult(r, "wildcard_bind_no_oidc") {
			t.Error("unexpected wildcard_bind_no_oidc when OIDC configured")
		}
	})
}

func TestCheckExternalURLNotHTTPS(t *testing.T) {
	t.Run("fires on HTTP", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{ExternalURL: "http://example.com"},
		}
		r := RunConfigChecks(cfg)
		if !hasResult(r, "external_url_not_https") {
			t.Error("expected external_url_not_https result")
		}
	})

	t.Run("silent on HTTPS", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{ExternalURL: "https://example.com"},
		}
		r := RunConfigChecks(cfg)
		if hasResult(r, "external_url_not_https") {
			t.Error("unexpected external_url_not_https on HTTPS")
		}
	})

	t.Run("silent when empty", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		if hasResult(r, "external_url_not_https") {
			t.Error("unexpected external_url_not_https when empty")
		}
	})
}

func TestCheckOpenbaoHTTP(t *testing.T) {
	t.Run("fires on HTTP", func(t *testing.T) {
		cfg := &config.Config{
			Openbao: &config.OpenbaoConfig{Address: "http://vault:8200"},
		}
		r := RunConfigChecks(cfg)
		if !hasResult(r, "openbao_http") {
			t.Error("expected openbao_http result")
		}
	})

	t.Run("silent on HTTPS", func(t *testing.T) {
		cfg := &config.Config{
			Openbao: &config.OpenbaoConfig{Address: "https://vault:8200"},
		}
		r := RunConfigChecks(cfg)
		if hasResult(r, "openbao_http") {
			t.Error("unexpected openbao_http on HTTPS")
		}
	})

	t.Run("silent when not configured", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		if hasResult(r, "openbao_http") {
			t.Error("unexpected openbao_http when not configured")
		}
	})
}

func TestCheckManagementBindPublic(t *testing.T) {
	t.Run("fires on wildcard", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{ManagementBind: "0.0.0.0:9090"},
		}
		r := RunConfigChecks(cfg)
		if !hasResult(r, "management_bind_public") {
			t.Error("expected management_bind_public result")
		}
	})

	t.Run("silent on loopback", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{ManagementBind: "127.0.0.1:9090"},
		}
		r := RunConfigChecks(cfg)
		if hasResult(r, "management_bind_public") {
			t.Error("unexpected management_bind_public on loopback")
		}
	})

	t.Run("silent when not configured", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		if hasResult(r, "management_bind_public") {
			t.Error("unexpected management_bind_public when not configured")
		}
	})
}

func TestCheckNoDefaultMemoryLimit(t *testing.T) {
	t.Run("fires when empty", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		if !hasResult(r, "no_default_memory_limit") {
			t.Error("expected no_default_memory_limit result")
		}
	})

	t.Run("silent when set", func(t *testing.T) {
		cfg := &config.Config{
			Docker: config.DockerConfig{DefaultMemoryLimit: "2g"},
		}
		r := RunConfigChecks(cfg)
		if hasResult(r, "no_default_memory_limit") {
			t.Error("unexpected no_default_memory_limit when set")
		}
	})
}

func TestCheckNoDefaultCPULimit(t *testing.T) {
	t.Run("fires when zero", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		if !hasResult(r, "no_default_cpu_limit") {
			t.Error("expected no_default_cpu_limit result")
		}
	})

	t.Run("silent when set", func(t *testing.T) {
		cfg := &config.Config{
			Docker: config.DockerConfig{DefaultCPULimit: 4.0},
		}
		r := RunConfigChecks(cfg)
		if hasResult(r, "no_default_cpu_limit") {
			t.Error("unexpected no_default_cpu_limit when set")
		}
	})
}

func TestCheckNoAuditLog(t *testing.T) {
	t.Run("fires when absent", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		res := findResult(r, "no_audit_log")
		if res == nil {
			t.Fatal("expected no_audit_log result")
			return
		}
		if res.Severity != SeverityInfo {
			t.Errorf("expected SeverityInfo, got %d", res.Severity)
		}
	})

	t.Run("silent when configured", func(t *testing.T) {
		cfg := &config.Config{
			Audit: &config.AuditConfig{Path: "/var/log/audit.jsonl"},
		}
		r := RunConfigChecks(cfg)
		if hasResult(r, "no_audit_log") {
			t.Error("unexpected no_audit_log when configured")
		}
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
		if !hasResult(r, "trusted_proxies_too_broad") {
			t.Error("expected trusted_proxies_too_broad result")
		}
	})

	t.Run("fires on /4", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{
				TrustedProxies: []string{"10.0.0.0/4"},
			},
		}
		r := RunConfigChecks(cfg)
		if !hasResult(r, "trusted_proxies_too_broad") {
			t.Error("expected trusted_proxies_too_broad for /4")
		}
	})

	t.Run("silent on /8", func(t *testing.T) {
		cfg := &config.Config{
			Server: config.ServerConfig{
				TrustedProxies: []string{"10.0.0.0/8"},
			},
		}
		r := RunConfigChecks(cfg)
		if hasResult(r, "trusted_proxies_too_broad") {
			t.Error("unexpected trusted_proxies_too_broad for /8")
		}
	})

	t.Run("silent when empty", func(t *testing.T) {
		cfg := &config.Config{}
		r := RunConfigChecks(cfg)
		if hasResult(r, "trusted_proxies_too_broad") {
			t.Error("unexpected trusted_proxies_too_broad when empty")
		}
	})
}

// --- helpers ---

func hasResult(r *Report, name string) bool {
	return findResult(r, name) != nil
}

func findResult(r *Report, name string) *Result {
	for i := range r.Results {
		if r.Results[i].Name == name {
			return &r.Results[i]
		}
	}
	return nil
}
