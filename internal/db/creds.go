package db

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// CredsRotator mints fresh Postgres credentials. Implementations must
// be safe for concurrent use — the startup path and the health poller
// may both invoke Rotate.
//
// The production implementation reads from vault's
// `{mount}/static-creds/{role}` endpoint; the db package keeps the
// interface narrow so it stays decoupled from internal/integration.
type CredsRotator interface {
	Rotate(ctx context.Context) (user, password string, err error)
}

// RotatorFunc adapts a bare function into a CredsRotator, so main.go
// can wire a vault closure without declaring a type.
type RotatorFunc func(ctx context.Context) (user, password string, err error)

func (f RotatorFunc) Rotate(ctx context.Context) (string, string, error) {
	return f(ctx)
}

// pgCredsProvider holds the current Postgres username/password and
// refreshes them on demand via a CredsRotator. Starts empty — callers
// are expected to Rotate() once at Open time before the first
// connection attempt when a rotator is configured.
//
// When no rotator is set, the provider stays empty forever and the
// pool's BeforeConnect hook becomes a no-op, leaving pgx to honor the
// DSN's userinfo.
type pgCredsProvider struct {
	mu       sync.Mutex
	user     string
	password string
	rotator  CredsRotator
}

func newPgCredsProvider(rot CredsRotator) *pgCredsProvider {
	return &pgCredsProvider{rotator: rot}
}

// current returns the current creds. Safe for concurrent use from
// pgxpool's BeforeConnect hook.
func (p *pgCredsProvider) current() (user, password string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.user, p.password
}

func (p *pgCredsProvider) hasRotator() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rotator != nil
}

// rotate fetches fresh creds and caches them. Returns an error if no
// rotator is configured or if the rotator call fails.
func (p *pgCredsProvider) rotate(ctx context.Context) error {
	p.mu.Lock()
	rot := p.rotator
	p.mu.Unlock()
	if rot == nil {
		return fmt.Errorf("no creds rotator configured")
	}

	user, pass, err := rot.Rotate(ctx)
	if err != nil {
		return fmt.Errorf("fetch db creds: %w", err)
	}

	p.mu.Lock()
	p.user = user
	p.password = pass
	p.mu.Unlock()
	slog.Info("fetched postgres credentials from vault", "user", user)
	return nil
}
