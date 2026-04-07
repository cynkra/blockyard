package preflight

import (
	"testing"

	"github.com/cynkra/blockyard/internal/config"
)

func TestTCPAddrFromRedisURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"redis://redis:6379", "redis:6379"},
		{"redis://host.example:6380/0", "host.example:6380"},
		{"redis://:password@host:6379", "host:6379"},
		{"redis://host", "host:6379"}, // port defaulted
		{"redis:///0", ""},            // empty host
		{":::bad:::", ""},             // unparseable
	}
	for _, tc := range cases {
		got := TCPAddrFromRedisURL(tc.in)
		if got != tc.want {
			t.Errorf("TCPAddrFromRedisURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTCPAddrFromHTTPURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"http://openbao:8200", "openbao:8200"},
		{"https://vault.example.com:8200", "vault.example.com:8200"},
		{"http://host", "host:80"},       // default HTTP port
		{"https://host", "host:443"},     // default HTTPS port
		{"ftp://host", ""},               // unsupported scheme
		{"http:///path", ""},             // no host
	}
	for _, tc := range cases {
		got := TCPAddrFromHTTPURL(tc.in)
		if got != tc.want {
			t.Errorf("TCPAddrFromHTTPURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTCPAddrFromDBConfig(t *testing.T) {
	cases := []struct {
		name string
		db   config.DatabaseConfig
		want string
	}{
		{"sqlite", config.DatabaseConfig{Driver: "sqlite", Path: "/data/db"}, ""},
		{"postgres URL", config.DatabaseConfig{
			Driver: "postgres",
			URL:    "postgres://user:pw@db.example.com:5433/mydb",
		}, "db.example.com:5433"},
		{"postgres URL default port", config.DatabaseConfig{
			Driver: "postgres",
			URL:    "postgres://user@host/db",
		}, "host:5432"},
		{"postgres keyval", config.DatabaseConfig{
			Driver: "postgres",
			URL:    "host=pg.internal port=5433 user=x dbname=y",
		}, "pg.internal:5433"},
		{"postgres keyval default port", config.DatabaseConfig{
			Driver: "postgres",
			URL:    "host=pg.internal user=x dbname=y",
		}, "pg.internal:5432"},
		{"postgres no host", config.DatabaseConfig{Driver: "postgres", URL: "user=x"}, ""},
		{"postgres empty URL", config.DatabaseConfig{Driver: "postgres"}, ""},
	}
	for _, tc := range cases {
		got := TCPAddrFromDBConfig(tc.db)
		if got != tc.want {
			t.Errorf("%s: TCPAddrFromDBConfig = %q, want %q", tc.name, got, tc.want)
		}
	}
}
