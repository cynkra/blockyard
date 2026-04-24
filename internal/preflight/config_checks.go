package preflight

import (
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/cynkra/blockyard/internal/config"
)

// RunConfigChecks evaluates checks that need only the parsed config.
// Safe to call immediately after config.Load().
func RunConfigChecks(cfg *config.Config) *Report {
	r := &Report{RanAt: time.Now().UTC()}

	// Check wildcard_bind_no_oidc subsumes no_oidc: when both fire,
	// only emit the more specific wildcard+no-OIDC warning.
	wildcardNoOIDC := checkWildcardBindNoOIDC(cfg)
	if wildcardNoOIDC.Severity > SeverityOK {
		r.Add(wildcardNoOIDC)
	} else {
		r.Add(checkNoOIDC(cfg))
	}

	r.Add(checkExternalURLNotHTTPS(cfg))
	r.Add(checkVaultHTTP(cfg))
	r.Add(checkVaultSecretIDFile(cfg))
	r.Add(checkManagementBindPublic(cfg))
	r.Add(checkNoDefaultMemoryLimit(cfg))
	r.Add(checkNoDefaultCPULimit(cfg))
	r.Add(checkNoAuditLog(cfg))
	r.Add(checkTrustedProxiesTooBroad(cfg))

	return r
}

// checkNoOIDC warns when the server runs without authentication.
func checkNoOIDC(cfg *config.Config) Result {
	if cfg.OIDC != nil {
		return Result{
			Name:     "no_oidc",
			Severity: SeverityOK,
			Message:  "OIDC authentication is configured",
			Category: "config",
		}
	}
	return Result{
		Name:     "no_oidc",
		Severity: SeverityWarning,
		Message:  "no [oidc] configured; server is completely unauthenticated",
		Category: "config",
	}
}

// checkWildcardBindNoOIDC warns when the server listens on all
// interfaces without any authentication — anyone on the network can
// deploy and manage apps.
func checkWildcardBindNoOIDC(cfg *config.Config) Result {
	if cfg.OIDC != nil {
		return Result{
			Name:     "wildcard_bind_no_oidc",
			Severity: SeverityOK,
			Message:  "OIDC configured; wildcard bind is safe",
			Category: "config",
		}
	}
	if !isWildcardBind(cfg.Server.Bind) {
		return Result{
			Name:     "wildcard_bind_no_oidc",
			Severity: SeverityOK,
			Message:  "server binds to a specific interface",
			Category: "config",
		}
	}
	return Result{
		Name:     "wildcard_bind_no_oidc",
		Severity: SeverityWarning,
		Message:  "server binds to all interfaces without [oidc]; anyone on the network has full access",
		Category: "config",
	}
}

// checkExternalURLNotHTTPS warns when external_url does not use HTTPS.
// Without HTTPS, session cookies lack the Secure flag, HSTS is not
// set, and OIDC tokens transit in cleartext.
func checkExternalURLNotHTTPS(cfg *config.Config) Result {
	if cfg.Server.ExternalURL == "" {
		return Result{
			Name:     "external_url_not_https",
			Severity: SeverityOK,
			Message:  "no external URL configured",
			Category: "config",
		}
	}
	if strings.HasPrefix(cfg.Server.ExternalURL, "https://") {
		return Result{
			Name:     "external_url_not_https",
			Severity: SeverityOK,
			Message:  "external URL uses HTTPS",
			Category: "config",
		}
	}
	return Result{
		Name:     "external_url_not_https",
		Severity: SeverityWarning,
		Message:  "server.external_url is not HTTPS; session cookies will lack the Secure flag and HSTS will not be set",
		Category: "config",
	}
}

// checkVaultHTTP warns when the vault address uses plain HTTP.
func checkVaultHTTP(cfg *config.Config) Result {
	if cfg.Vault == nil {
		return Result{
			Name:     "vault_http",
			Severity: SeverityOK,
			Message:  "vault not configured",
			Category: "config",
		}
	}
	if !strings.HasPrefix(cfg.Vault.Address, "http://") {
		return Result{
			Name:     "vault_http",
			Severity: SeverityOK,
			Message:  "vault address uses HTTPS",
			Category: "config",
		}
	}
	return Result{
		Name:     "vault_http",
		Severity: SeverityWarning,
		Message:  "vault.address uses plain HTTP; vault traffic (tokens, secrets) is not encrypted",
		Category: "config",
	}
}

// checkVaultSecretIDFile errors when vault.secret_id_file points at a
// file that other uids on the host can read. On the process backend,
// workers run as unprivileged sibling uids; a mode-0644 or
// group-readable secret_id file leaks the AppRole secret_id to any
// of them. We require mode bits 0077 to be clear and the file to be
// owned by blockyard's effective uid.
func checkVaultSecretIDFile(cfg *config.Config) Result {
	const name = "vault_secret_id_file"
	const category = "config"

	if cfg.Vault == nil || cfg.Vault.SecretIDFile == "" {
		return Result{
			Name:     name,
			Severity: SeverityOK,
			Message:  "vault.secret_id_file not configured",
			Category: category,
		}
	}

	path := cfg.Vault.SecretIDFile
	info, err := os.Stat(path)
	if err != nil {
		return Result{
			Name:     name,
			Severity: SeverityError,
			Message:  fmt.Sprintf("vault.secret_id_file %q: %v", path, err),
			Category: category,
		}
	}
	if !info.Mode().IsRegular() {
		return Result{
			Name:     name,
			Severity: SeverityError,
			Message:  fmt.Sprintf("vault.secret_id_file %q is not a regular file", path),
			Category: category,
		}
	}

	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return Result{
			Name:     name,
			Severity: SeverityError,
			Message: fmt.Sprintf(
				"vault.secret_id_file %q has mode %#o (group/other-accessible); clear group and other bits (e.g. `chmod 0400 %s` or 0600)",
				path, mode, path),
			Category: category,
		}
	}

	// info.Sys() returns *syscall.Stat_t on Linux; on non-Linux GOOS the
	// assertion fails and we skip the uid check. Blockyard targets Linux
	// in production, so this branch is effectively always taken.
	stat, ok := info.Sys().(*syscall.Stat_t)
	if ok {
		euid := uint32(os.Geteuid()) //nolint:gosec // G115: Geteuid is non-negative on Linux
		if stat.Uid != euid {
			return Result{
				Name:     name,
				Severity: SeverityError,
				Message: fmt.Sprintf(
					"vault.secret_id_file %q is owned by uid %d, not process uid %d; run `chown %d %s`",
					path, stat.Uid, euid, euid, path),
				Category: category,
			}
		}
	}

	return Result{
		Name:     name,
		Severity: SeverityOK,
		Message:  fmt.Sprintf("vault.secret_id_file %q has safe permissions", path),
		Category: category,
	}
}

// checkManagementBindPublic warns when the management listener binds
// to a wildcard address. The management router serves /healthz,
// /readyz, and /metrics without authentication.
func checkManagementBindPublic(cfg *config.Config) Result {
	if cfg.Server.ManagementBind == "" {
		return Result{
			Name:     "management_bind_public",
			Severity: SeverityOK,
			Message:  "no management listener configured",
			Category: "config",
		}
	}
	if !isWildcardBind(cfg.Server.ManagementBind) {
		return Result{
			Name:     "management_bind_public",
			Severity: SeverityOK,
			Message:  "management listener binds to a specific interface",
			Category: "config",
		}
	}
	return Result{
		Name:     "management_bind_public",
		Severity: SeverityWarning,
		Message:  "server.management_bind listens on all interfaces; /healthz, /readyz, /metrics are unauthenticated",
		Category: "config",
	}
}

// checkNoDefaultMemoryLimit warns when no default memory limit is set
// for workers. A single runaway app can OOM the host.
func checkNoDefaultMemoryLimit(cfg *config.Config) Result {
	if cfg.Server.DefaultMemoryLimit != "" {
		return Result{
			Name:     "no_default_memory_limit",
			Severity: SeverityOK,
			Message:  "default memory limit is set",
			Category: "config",
		}
	}
	return Result{
		Name:     "no_default_memory_limit",
		Severity: SeverityWarning,
		Message:  "server.default_memory_limit is not set; workers have no memory limit",
		Category: "config",
	}
}

// checkNoDefaultCPULimit warns when no default CPU limit is set for
// workers. A single app can starve all others.
func checkNoDefaultCPULimit(cfg *config.Config) Result {
	if cfg.Server.DefaultCPULimit != 0 {
		return Result{
			Name:     "no_default_cpu_limit",
			Severity: SeverityOK,
			Message:  "default CPU limit is set",
			Category: "config",
		}
	}
	return Result{
		Name:     "no_default_cpu_limit",
		Severity: SeverityWarning,
		Message:  "server.default_cpu_limit is not set; workers have no CPU limit",
		Category: "config",
	}
}

// checkNoAuditLog notes when audit logging is not configured.
func checkNoAuditLog(cfg *config.Config) Result {
	if cfg.Audit != nil {
		return Result{
			Name:     "no_audit_log",
			Severity: SeverityOK,
			Message:  "audit logging is configured",
			Category: "config",
		}
	}
	return Result{
		Name:     "no_audit_log",
		Severity: SeverityInfo,
		Message:  "no [audit] configured; operations will not be logged to an audit trail",
		Category: "config",
	}
}

// checkTrustedProxiesTooBroad warns about absurdly broad trusted proxy
// CIDRs that effectively let any client spoof their IP.
func checkTrustedProxiesTooBroad(cfg *config.Config) Result {
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
			return Result{
				Name:     "trusted_proxies_too_broad",
				Severity: SeverityWarning,
				Message:  "server.trusted_proxies contains " + cidr + " which is extremely broad; any client can spoof their IP and bypass rate limits",
				Category: "config",
			}
		}
	}
	return Result{
		Name:     "trusted_proxies_too_broad",
		Severity: SeverityOK,
		Message:  "trusted proxy CIDRs are reasonable",
		Category: "config",
	}
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
