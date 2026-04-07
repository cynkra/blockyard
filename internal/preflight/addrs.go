package preflight

import (
	"net"
	"net/url"
	"strings"

	"github.com/cynkra/blockyard/internal/config"
)

// TCPAddrFromRedisURL extracts host:port from a redis:// URL.
// Returns empty string if the URL cannot be parsed or has no host.
// Defaults to port 6379 if the URL omits a port.
func TCPAddrFromRedisURL(redisURL string) string {
	if redisURL == "" {
		return ""
	}
	u, err := url.Parse(redisURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "6379"
	}
	return net.JoinHostPort(host, port)
}

// TCPAddrFromHTTPURL extracts host:port from an http:// or https:// URL.
// Returns empty string if the URL cannot be parsed or has no host.
// Defaults to 80 for http and 443 for https when the URL omits a port.
func TCPAddrFromHTTPURL(httpURL string) string {
	if httpURL == "" {
		return ""
	}
	u, err := url.Parse(httpURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return ""
		}
	}
	return net.JoinHostPort(host, port)
}

// TCPAddrFromDBConfig returns host:port for the configured database, or
// "" if the database is local-only (SQLite, or postgres without a TCP
// address). For PostgreSQL it parses the libpq DSN form
// `postgres://user:pass@host:port/db?...` or the `key=value` form
// (`host=... port=...`).
func TCPAddrFromDBConfig(db config.DatabaseConfig) string {
	if db.Driver != "postgres" {
		return ""
	}
	if db.URL == "" {
		return ""
	}
	if strings.HasPrefix(db.URL, "postgres://") || strings.HasPrefix(db.URL, "postgresql://") {
		u, err := url.Parse(db.URL)
		if err != nil {
			return ""
		}
		host := u.Hostname()
		if host == "" {
			return ""
		}
		port := u.Port()
		if port == "" {
			port = "5432"
		}
		return net.JoinHostPort(host, port)
	}
	// Key=value form: host=... port=...
	var host, port string
	for _, field := range strings.Fields(db.URL) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch k {
		case "host":
			host = v
		case "port":
			port = v
		}
	}
	if host == "" {
		return ""
	}
	if port == "" {
		port = "5432"
	}
	return net.JoinHostPort(host, port)
}
