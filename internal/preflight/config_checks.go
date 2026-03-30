package preflight

import (
	"net"
	"strings"

	"github.com/cynkra/blockyard/internal/config"
)

// RunConfigChecks evaluates checks that need only the parsed config.
// Safe to call immediately after config.Load().
func RunConfigChecks(cfg *config.Config) *Report {
	r := &Report{}

	// Check 5 subsumes check 4: when both fire, only emit the more
	// specific wildcard+no-OIDC warning.
	wildcardNoOIDC := checkWildcardBindNoOIDC(cfg)
	if wildcardNoOIDC != nil {
		r.add(wildcardNoOIDC)
	} else {
		r.add(checkNoOIDC(cfg))
	}

	r.add(checkExternalURLNotHTTPS(cfg))
	r.add(checkOpenbaoHTTP(cfg))
	r.add(checkManagementBindPublic(cfg))
	r.add(checkNoDefaultMemoryLimit(cfg))
	r.add(checkNoDefaultCPULimit(cfg))
	r.add(checkNoAuditLog(cfg))
	r.add(checkTrustedProxiesTooBroad(cfg))

	return r
}

// checkNoOIDC warns when the server runs without authentication.
func checkNoOIDC(cfg *config.Config) *Result {
	if cfg.OIDC != nil {
		return nil
	}
	return &Result{
		Name:     "no_oidc",
		Severity: SeverityWarning,
		Message:  "no [oidc] configured; server is completely unauthenticated",
	}
}

// checkWildcardBindNoOIDC warns when the server listens on all
// interfaces without any authentication — anyone on the network can
// deploy and manage apps.
func checkWildcardBindNoOIDC(cfg *config.Config) *Result {
	if cfg.OIDC != nil {
		return nil
	}
	if !isWildcardBind(cfg.Server.Bind) {
		return nil
	}
	return &Result{
		Name:     "wildcard_bind_no_oidc",
		Severity: SeverityWarning,
		Message:  "server binds to all interfaces without [oidc]; anyone on the network has full access",
	}
}

// checkExternalURLNotHTTPS warns when external_url does not use HTTPS.
// Without HTTPS, session cookies lack the Secure flag, HSTS is not
// set, and OIDC tokens transit in cleartext.
func checkExternalURLNotHTTPS(cfg *config.Config) *Result {
	if cfg.Server.ExternalURL == "" {
		return nil
	}
	if strings.HasPrefix(cfg.Server.ExternalURL, "https://") {
		return nil
	}
	return &Result{
		Name:     "external_url_not_https",
		Severity: SeverityWarning,
		Message:  "server.external_url is not HTTPS; session cookies will lack the Secure flag and HSTS will not be set",
	}
}

// checkOpenbaoHTTP warns when the OpenBao/Vault address uses plain HTTP.
func checkOpenbaoHTTP(cfg *config.Config) *Result {
	if cfg.Openbao == nil {
		return nil
	}
	if !strings.HasPrefix(cfg.Openbao.Address, "http://") {
		return nil
	}
	return &Result{
		Name:     "openbao_http",
		Severity: SeverityWarning,
		Message:  "openbao.address uses plain HTTP; vault traffic (tokens, secrets) is not encrypted",
	}
}

// checkManagementBindPublic warns when the management listener binds
// to a wildcard address. The management router serves /healthz,
// /readyz, and /metrics without authentication.
func checkManagementBindPublic(cfg *config.Config) *Result {
	if cfg.Server.ManagementBind == "" {
		return nil
	}
	if !isWildcardBind(cfg.Server.ManagementBind) {
		return nil
	}
	return &Result{
		Name:     "management_bind_public",
		Severity: SeverityWarning,
		Message:  "server.management_bind listens on all interfaces; /healthz, /readyz, /metrics are unauthenticated",
	}
}

// checkNoDefaultMemoryLimit warns when no default memory limit is set
// for worker containers. A single runaway app can OOM the host.
func checkNoDefaultMemoryLimit(cfg *config.Config) *Result {
	if cfg.Docker.DefaultMemoryLimit != "" {
		return nil
	}
	return &Result{
		Name:     "no_default_memory_limit",
		Severity: SeverityWarning,
		Message:  "docker.default_memory_limit is not set; worker containers have no memory limit",
	}
}

// checkNoDefaultCPULimit warns when no default CPU limit is set for
// worker containers. A single app can starve all others.
func checkNoDefaultCPULimit(cfg *config.Config) *Result {
	if cfg.Docker.DefaultCPULimit != 0 {
		return nil
	}
	return &Result{
		Name:     "no_default_cpu_limit",
		Severity: SeverityWarning,
		Message:  "docker.default_cpu_limit is not set; worker containers have no CPU limit",
	}
}

// checkNoAuditLog notes when audit logging is not configured.
func checkNoAuditLog(cfg *config.Config) *Result {
	if cfg.Audit != nil {
		return nil
	}
	return &Result{
		Name:     "no_audit_log",
		Severity: SeverityInfo,
		Message:  "no [audit] configured; operations will not be logged to an audit trail",
	}
}

// checkTrustedProxiesTooBroad warns about absurdly broad trusted proxy
// CIDRs that effectively let any client spoof their IP.
func checkTrustedProxiesTooBroad(cfg *config.Config) *Result {
	for _, cidr := range cfg.Server.TrustedProxies {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue // syntax errors caught by validate()
		}
		ones, bits := ipNet.Mask.Size()
		if bits == 0 {
			continue // non-standard mask
		}
		// IPv4 ≤ /4 or IPv6 ≤ /8 covers an unreasonably large portion
		// of the address space.
		maxPrefix := 4
		if bits == 128 {
			maxPrefix = 8
		}
		if ones <= maxPrefix {
			return &Result{
				Name:     "trusted_proxies_too_broad",
				Severity: SeverityWarning,
				Message:  "server.trusted_proxies contains " + cidr + " which is extremely broad; any client can spoof their IP and bypass rate limits",
			}
		}
	}
	return nil
}

// isWildcardBind returns true if the bind address listens on all
// interfaces (0.0.0.0 or ::).
func isWildcardBind(bind string) bool {
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		host = bind
	}
	return host == "0.0.0.0" || host == "::" || host == ""
}
