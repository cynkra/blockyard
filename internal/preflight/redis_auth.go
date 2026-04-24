package preflight

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/cynkra/blockyard/internal/config"
)

// CheckRedisAuth dials the configured Redis URL without AUTH, sends
// a PING, and classifies the reply. Applies across backends —
// unauth'd Redis is a footgun whether workers live in a Docker
// network namespace, a bwrap sandbox, or a host-network process.
//
// Severities:
//   - SeverityOK: Redis replies `-NOAUTH` (authentication required).
//   - SeverityError: Redis replies `+PONG` (auth not required).
//   - SeverityInfo: TLS Redis (`rediss://` — plain-TCP probe skipped),
//     Redis not reachable, generic unexpected reply. Surfaced as Info
//     so the operator investigates rather than getting a silent OK.
//
// Called explicitly from both backends' RunPreflight so both coverage
// modes benefit from the same check. Two call sites are small enough
// not to warrant a registry abstraction; revisit if a third backend
// ships.
func CheckRedisAuth(cfg *config.RedisConfig) Result {
	const name = "redis_auth"
	if cfg == nil || cfg.URL == "" {
		return Result{
			Name: name, Severity: SeverityOK,
			Message: "Redis not configured", Category: "redis",
		}
	}
	if strings.HasPrefix(strings.ToLower(cfg.URL), "rediss://") {
		// Plain-TCP probe against a TLS-only server would write
		// garbage at the TLS handshake layer and get no useful reply.
		// Skip rather than report spurious "unexpected reply".
		return Result{
			Name: name, Severity: SeverityInfo,
			Message:  "TLS Redis (rediss://); plain-TCP auth probe skipped",
			Category: "redis",
		}
	}
	hp := TCPAddrFromRedisURL(cfg.URL)
	if hp == "" {
		return Result{
			Name: name, Severity: SeverityInfo,
			Message:  "Redis URL not parseable for auth probe",
			Category: "redis",
		}
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.Dial("tcp", hp)
	if err != nil {
		return Result{
			Name: name, Severity: SeverityInfo,
			Message:  "Redis not reachable from blockyard for auth probe",
			Category: "redis",
		}
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return Result{
			Name: name, Severity: SeverityInfo,
			Message:  fmt.Sprintf("Redis PING failed: %v", err),
			Category: "redis",
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	buf := make([]byte, 128)
	n, _ := conn.Read(buf)
	reply := string(buf[:n])
	switch {
	case strings.HasPrefix(reply, "+PONG"):
		return Result{
			Name: name, Severity: SeverityError,
			Message: "Redis accepts commands without authentication. " +
				"Any host-network process (including compromised workers) can " +
				"read/modify session state, flush the registry, or DoS the service. " +
				"Configure `requirepass` in redis.conf or enable ACLs.",
			Category: "redis",
		}
	case strings.HasPrefix(reply, "-NOAUTH"):
		return Result{
			Name: name, Severity: SeverityOK,
			Message: "Redis requires authentication", Category: "redis",
		}
	default:
		// Generic `-ERR ...` (MAXCLIENTS, protocol errors), an empty
		// reply, or surprise `-WRONGPASS` / `-NOPERM` from ACL servers
		// that shouldn't fire for an unauthenticated PING. Surface as
		// Info so the operator investigates rather than getting a
		// false OK.
		return Result{
			Name: name, Severity: SeverityInfo,
			Message: fmt.Sprintf(
				"Redis responded with unexpected reply to unauthenticated PING: %q", reply),
			Category: "redis",
		}
	}
}
