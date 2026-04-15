package db

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
)

// CredsRotator mints fresh Postgres credentials. Implementations are
// expected to be thread-safe; Rotate may be called concurrently by the
// startup fallback path and the health-poller mid-run path.
//
// The typical implementation calls OpenBao's database secrets engine.
// The db package keeps the interface narrow so internal/db stays
// decoupled from internal/integration.
type CredsRotator interface {
	Rotate(ctx context.Context) (user, password string, err error)
}

// RotatorFunc adapts a bare function into a CredsRotator. Useful for
// wiring an OpenBao closure from main.go without a dedicated struct.
type RotatorFunc func(ctx context.Context) (user, password string, err error)

func (f RotatorFunc) Rotate(ctx context.Context) (string, string, error) {
	return f(ctx)
}

// pgCredsProvider holds the current Postgres username/password and
// rotates them on demand via a CredsRotator. Seeds from the DSN so
// the initial connection attempt uses the env-provided creds; if
// that fails and a rotator is configured, callers can trigger a
// rotation to mint fresh creds.
type pgCredsProvider struct {
	mu       sync.Mutex
	user     string
	password string
	rotator  CredsRotator
}

func newPgCredsProvider(dsnUser, dsnPass string, rot CredsRotator) *pgCredsProvider {
	return &pgCredsProvider{
		user:     dsnUser,
		password: dsnPass,
		rotator:  rot,
	}
}

// current returns the current user/password. Safe for concurrent use
// from pgxpool's BeforeConnect hook and external callers.
func (p *pgCredsProvider) current() (user, password string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.user, p.password
}

// hasRotator reports whether a rotator is configured. Callers use
// this to decide whether an auth-failure fallback is possible.
func (p *pgCredsProvider) hasRotator() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rotator != nil
}

// rotate fetches fresh credentials from the rotator and updates the
// cached values. Returns an error if no rotator is configured or if
// the rotator call fails.
func (p *pgCredsProvider) rotate(ctx context.Context) error {
	p.mu.Lock()
	rot := p.rotator
	p.mu.Unlock()
	if rot == nil {
		return fmt.Errorf("no creds rotator configured")
	}

	user, pass, err := rot.Rotate(ctx)
	if err != nil {
		return fmt.Errorf("mint db creds: %w", err)
	}

	p.mu.Lock()
	p.user = user
	p.password = pass
	p.mu.Unlock()
	slog.Info("rotated postgres credentials from vault", "user", user)
	return nil
}

// parseDSNUser extracts the user and password from a PostgreSQL URL.
// Returns empty strings if the URL has no userinfo. Errors on
// malformed URLs — caller should treat that as a config error.
func parseDSNUser(dsn string) (user, pass string, err error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", "", fmt.Errorf("parse postgres url: %w", err)
	}
	if u.User != nil {
		p, _ := u.User.Password()
		return u.User.Username(), p, nil
	}
	return "", "", nil
}
