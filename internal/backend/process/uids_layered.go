package process

import (
	"context"
	"fmt"
)

// layeredUIDAllocator pairs a Postgres-primary allocator with a Redis
// mirror (see #288, parent #262). See ports_layered.go for the
// reasoning — the structure is identical, just simpler because UIDs
// have no kernel-probe analog.
type layeredUIDAllocator struct {
	primary *postgresUIDAllocator
	cache   *redisUIDAllocator
}

func newLayeredUIDAllocator(primary *postgresUIDAllocator, cache *redisUIDAllocator) *layeredUIDAllocator {
	return &layeredUIDAllocator{primary: primary, cache: cache}
}

func (u *layeredUIDAllocator) Alloc() (int, error) {
	uid, err := u.primary.Alloc()
	if err != nil {
		return 0, err
	}
	u.cache.mirrorClaim(uid)
	return uid, nil
}

func (u *layeredUIDAllocator) Release(uid int) {
	u.primary.Release(uid)
	u.cache.Release(uid)
}

func (u *layeredUIDAllocator) InUse() int {
	return u.cache.InUse()
}

func (u *layeredUIDAllocator) CleanupOwnedOrphans(ctx context.Context) error {
	if err := u.primary.CleanupOwnedOrphans(ctx); err != nil {
		return fmt.Errorf("primary: %w", err)
	}
	if err := u.cache.CleanupOwnedOrphans(ctx); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}
