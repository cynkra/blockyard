package process

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// redisPortAllocator coordinates port allocation across blockyard
// peers via Redis. Keys are "{prefix}port:<N>" → "<hostname>"; claim
// is a single Lua SETNX scan, release is an ownership-checked DEL,
// and the allocator layers a kernel-probe retry loop on top so
// non-blockyard host processes holding a port in the range can be
// bypassed without leaking the Redis claim.
//
// The hostname field is the crash-recovery owner ID — not the
// workermap's per-process serverID. These identifiers solve
// different problems: hostname-scoped "clean up my host's crashed
// state" versus per-process "distinguish concurrent peers." Keeping
// them separate is deliberate.
type redisPortAllocator struct {
	client   *redisstate.Client
	first    int
	last     int
	hostname string
}

func newRedisPortAllocator(client *redisstate.Client, first, last int, hostname string) *redisPortAllocator {
	return &redisPortAllocator{
		client:   client,
		first:    first,
		last:     last,
		hostname: hostname,
	}
}

// portClaimScript is the Lua SETNX scan: start at max(first,
// skip_from), return the first port that claims successfully, or -1
// if the range is exhausted. The skip_from argument lets the Go
// caller advance past an externally-held port without mutating state.
var portClaimScript = redis.NewScript(`
local prefix = KEYS[1]
local first = tonumber(ARGV[1])
local last = tonumber(ARGV[2])
local hostname = ARGV[3]
local skip_from = tonumber(ARGV[4])
local from = first
if skip_from > from then
    from = skip_from
end
for i = from, last do
    local key = prefix .. "port:" .. i
    if redis.call("SETNX", key, hostname) == 1 then
        return i
    end
end
return -1
`)

// ownerDeleteScript is the shared ownership-checked DEL — the ports
// and UIDs variants both use it with different key prefixes. It
// prevents a hostname mismatch from accidentally deleting a peer's
// key (defensive; in the common case Release only runs on keys the
// local server itself allocated).
var ownerDeleteScript = redis.NewScript(`
local key = KEYS[1]
local owner = ARGV[1]
if redis.call("GET", key) == owner then
    return redis.call("DEL", key)
end
return 0
`)

// Reserve picks a free port by claiming it in Redis, then attempts to
// bind a listener. On bind failure (a non-blockyard host process
// holds the port), the Redis claim is released and the scan advances
// past the failed index via skip_from. Worst case: O(range) Lua calls.
func (p *redisPortAllocator) Reserve() (int, net.Listener, error) {
	skipFrom := 0
	for {
		port, err := p.luaAlloc(skipFrom)
		if err != nil {
			return 0, nil, fmt.Errorf("redis port alloc: %w", err)
		}
		if port < 0 {
			return 0, nil, errors.New("process backend: no free ports in range")
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return port, ln, nil
		}
		// Kernel says this port is externally busy. Drop the Redis
		// claim so future allocs (after the external holder
		// releases) can still use it, and advance skip_from so the
		// same index isn't re-probed in the current loop.
		_ = p.luaRelease(port)
		skipFrom = port + 1
	}
}

// Release returns a port to the pool via the ownership-checked DEL.
func (p *redisPortAllocator) Release(port int) {
	if port < p.first || port > p.last {
		return
	}
	if err := p.luaRelease(port); err != nil {
		slog.Error("redis port release",
			"port", port, "error", err)
	}
}

// InUse scans the port key namespace and counts entries owned by
// this host. Used by tests and diagnostic endpoints. Not load-
// bearing for correctness.
func (p *redisPortAllocator) InUse() int {
	ctx := context.Background()
	prefix := p.client.Prefix() + "port:"
	pattern := prefix + "*"
	var cursor uint64
	n := 0
	for {
		keys, next, err := p.client.Redis().Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			slog.Error("redis port in-use scan", "error", err)
			return n
		}
		for _, key := range keys {
			owner, err := p.client.Redis().Get(ctx, key).Result()
			if err == nil && owner == p.hostname {
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

func (p *redisPortAllocator) luaAlloc(skipFrom int) (int, error) {
	ctx := context.Background()
	res, err := portClaimScript.Run(ctx, p.client.Redis(),
		[]string{p.client.Prefix()},
		p.first, p.last, p.hostname, skipFrom,
	).Int()
	if err != nil {
		return 0, err
	}
	return res, nil
}

// mirrorClaim writes the claim key unconditionally for a specific
// port. Used by the layered allocator after Postgres has already
// arbitrated ownership — no SETNX needed because the primary mutex
// guarantees only one peer reaches this point with this port. Errors
// are logged here (matching the rest of this store's best-effort
// pattern) so callers don't have to repeat the slog dance.
func (p *redisPortAllocator) mirrorClaim(port int) {
	ctx := context.Background()
	key := p.client.Prefix() + "port:" + strconv.Itoa(port)
	if err := p.client.Redis().Set(ctx, key, p.hostname, 0).Err(); err != nil {
		slog.Warn("redis port mirror", "port", port, "error", err)
	}
}

func (p *redisPortAllocator) luaRelease(port int) error {
	ctx := context.Background()
	key := p.client.Prefix() + "port:" + strconv.Itoa(port)
	return ownerDeleteScript.Run(ctx, p.client.Redis(),
		[]string{key}, p.hostname).Err()
}

// CleanupOwnedOrphans scans the port key namespace, finds keys owned
// by this hostname, and deletes those whose index is not in the
// active workers map. Called at startup to reclaim slots a previous
// crashed instance left behind.
//
// The second argument is the set of ports currently held by live
// workers on this instance (which we should not delete). Process
// backend passes an empty set at startup because workers from the
// previous run are dead — Pdeathsig killed them with the server.
func (p *redisPortAllocator) CleanupOwnedOrphans(ctx context.Context) error {
	prefix := p.client.Prefix() + "port:"
	pattern := prefix + "*"
	var cursor uint64
	for {
		keys, next, err := p.client.Redis().Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		for _, key := range keys {
			owner, err := p.client.Redis().Get(ctx, key).Result()
			if err != nil {
				continue
			}
			if owner != p.hostname {
				continue
			}
			// Parse the port number out of the key suffix.
			suffix := strings.TrimPrefix(key, prefix)
			_, parseErr := strconv.Atoi(suffix)
			if parseErr != nil {
				continue
			}
			// Startup cleanup is unconditional for owned keys:
			// workers from the previous run are dead (Pdeathsig
			// killed them with the server).
			if err := ownerDeleteScript.Run(ctx, p.client.Redis(),
				[]string{key}, p.hostname).Err(); err != nil {
				slog.Warn("redis port cleanup: delete failed",
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
