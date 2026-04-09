package process

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// redisUIDAllocator coordinates UID allocation across blockyard
// peers via Redis. Same pattern as redisPortAllocator but simpler —
// UIDs have no kernel-side probe analogous to net.Listen, so the
// Alloc path is straight-line (no retry loop).
//
// Without this, a rolling-update cutover would deterministically
// collide UIDs: the new server's fresh bitset has no way to know UID
// 60000 is already in use by an old-server worker. With Redis
// coordination, each UID is claimed once across all peers on the
// host.
type redisUIDAllocator struct {
	client   *redisstate.Client
	first    int
	last     int
	hostname string
}

func newRedisUIDAllocator(client *redisstate.Client, first, last int, hostname string) *redisUIDAllocator {
	return &redisUIDAllocator{
		client:   client,
		first:    first,
		last:     last,
		hostname: hostname,
	}
}

// uidClaimScript is a SETNX scan over the UID range. Returns the
// first UID that claimed successfully, or -1 if the range is
// exhausted.
var uidClaimScript = redis.NewScript(`
local prefix = KEYS[1]
local first = tonumber(ARGV[1])
local last = tonumber(ARGV[2])
local hostname = ARGV[3]
for i = first, last do
    local key = prefix .. "uid:" .. i
    if redis.call("SETNX", key, hostname) == 1 then
        return i
    end
end
return -1
`)

// Alloc claims the next free UID via a single atomic Lua call.
func (u *redisUIDAllocator) Alloc() (int, error) {
	ctx := context.Background()
	res, err := uidClaimScript.Run(ctx, u.client.Redis(),
		[]string{u.client.Prefix()},
		u.first, u.last, u.hostname,
	).Int()
	if err != nil {
		return 0, fmt.Errorf("redis uid alloc: %w", err)
	}
	if res < 0 {
		return 0, errors.New("process backend: no free UIDs in range")
	}
	return res, nil
}

// Release returns a UID to the pool via the shared ownership-checked
// DEL script. No-op for out-of-range UIDs or keys not owned by this
// host.
func (u *redisUIDAllocator) Release(uid int) {
	if uid < u.first || uid > u.last {
		return
	}
	ctx := context.Background()
	key := u.client.Prefix() + "uid:" + strconv.Itoa(uid)
	if err := ownerDeleteScript.Run(ctx, u.client.Redis(),
		[]string{key}, u.hostname).Err(); err != nil {
		slog.Error("redis uid release",
			"uid", uid, "error", err)
	}
}

// InUse scans the UID key namespace and counts entries owned by this
// host.
func (u *redisUIDAllocator) InUse() int {
	ctx := context.Background()
	prefix := u.client.Prefix() + "uid:"
	pattern := prefix + "*"
	var cursor uint64
	n := 0
	for {
		keys, next, err := u.client.Redis().Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			slog.Error("redis uid in-use scan", "error", err)
			return n
		}
		for _, key := range keys {
			owner, err := u.client.Redis().Get(ctx, key).Result()
			if err == nil && owner == u.hostname {
				n++
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return n
}

// CleanupOwnedOrphans scans the UID key namespace and deletes entries
// owned by this hostname. The process backend's workers from a
// previous run are dead (Pdeathsig), so all owned keys at startup
// are stale.
func (u *redisUIDAllocator) CleanupOwnedOrphans(ctx context.Context) error {
	prefix := u.client.Prefix() + "uid:"
	pattern := prefix + "*"
	var cursor uint64
	for {
		keys, next, err := u.client.Redis().Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		for _, key := range keys {
			owner, err := u.client.Redis().Get(ctx, key).Result()
			if err != nil {
				continue
			}
			if owner != u.hostname {
				continue
			}
			suffix := strings.TrimPrefix(key, prefix)
			if _, parseErr := strconv.Atoi(suffix); parseErr != nil {
				continue
			}
			if err := ownerDeleteScript.Run(ctx, u.client.Redis(),
				[]string{key}, u.hostname).Err(); err != nil {
				slog.Warn("redis uid cleanup: delete failed",
					"key", key, "error", err)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return nil
}
