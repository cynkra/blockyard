package process

import (
	"context"
	"fmt"
	"log/slog"
	"net"
)

// layeredPortAllocator pairs a Postgres-primary allocator with a Redis
// mirror (see #288, parent #262). Postgres is the cross-peer mutex —
// the unique constraint on blockyard_ports is what prevents two peers
// from claiming the same slot. Redis is a best-effort mirror so that
// peers querying InUse hit the cache instead of doing a Postgres scan.
type layeredPortAllocator struct {
	primary *postgresPortAllocator
	cache   *redisPortAllocator
}

func newLayeredPortAllocator(primary *postgresPortAllocator, cache *redisPortAllocator) *layeredPortAllocator {
	return &layeredPortAllocator{primary: primary, cache: cache}
}

// Reserve claims via Postgres (the mutex), then mirrors the claim to
// Redis. On a kernel-bound retry inside the primary's Reserve loop,
// the primary handles the DB cleanup itself — the cache only ever
// sees successfully-bound ports.
func (p *layeredPortAllocator) Reserve() (int, net.Listener, error) {
	port, ln, err := p.primary.Reserve()
	if err != nil {
		return 0, nil, err
	}
	p.cache.mirrorClaim(port)
	return port, ln, nil
}

func (p *layeredPortAllocator) Release(port int) {
	p.primary.Release(port)
	p.cache.Release(port)
}

// InUse reports the cache's count. Both layers report counts owned by
// the local hostname; in steady state they agree, and InUse is a
// diagnostic rather than load-bearing for correctness, so a transient
// cache miss surfaces honestly as 0 rather than masquerading as a
// primary read.
func (p *layeredPortAllocator) InUse() int {
	return p.cache.InUse()
}

// CleanupOwnedOrphans cleans the primary (correctness). The cache mirror
// is cleaned best-effort: a Redis outage at startup must not block
// recovery of orphaned Postgres rows, and any stale cache keys will
// naturally churn as live workers reclaim their slots. Matches the
// primary/cache contract in ports_layered.go's header.
func (p *layeredPortAllocator) CleanupOwnedOrphans(ctx context.Context) error {
	if err := p.primary.CleanupOwnedOrphans(ctx); err != nil {
		return fmt.Errorf("primary: %w", err)
	}
	if err := p.cache.CleanupOwnedOrphans(ctx); err != nil {
		slog.Warn("layered port cleanup: cache best-effort", "error", err)
	}
	return nil
}
