package registry

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// RedisRegistry implements WorkerRegistry using simple Redis strings.
//
// Key schema:
//
//	{prefix}registry:{workerID}  ->  string "host:port"
//
// Each key has a TTL equal to registryTTL. The health poller refreshes
// the TTL on every successful check by calling Set again.
type RedisRegistry struct {
	client      *redisstate.Client
	registryTTL time.Duration
}

func NewRedisRegistry(client *redisstate.Client, registryTTL time.Duration) *RedisRegistry {
	return &RedisRegistry{client: client, registryTTL: registryTTL}
}

func (r *RedisRegistry) registryKey(workerID string) string {
	return r.client.Key("registry", workerID)
}

func (r *RedisRegistry) Get(workerID string) (string, bool) {
	ctx := context.Background()
	addr, err := r.client.Redis().Get(ctx, r.registryKey(workerID)).Result()
	if err == redis.Nil {
		return "", false
	}
	if err != nil {
		slog.Error("redis registry get", "worker_id", workerID, "error", err)
		return "", false
	}
	return addr, true
}

func (r *RedisRegistry) Set(workerID, addr string) {
	ctx := context.Background()
	key := r.registryKey(workerID)
	pipe := r.client.Redis().Pipeline()
	pipe.Set(ctx, key, addr, 0)
	if r.registryTTL > 0 {
		pipe.Expire(ctx, key, r.registryTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Error("redis registry set", "worker_id", workerID, "error", err)
	}
}

func (r *RedisRegistry) Delete(workerID string) {
	ctx := context.Background()
	if err := r.client.Redis().Del(ctx, r.registryKey(workerID)).Err(); err != nil {
		slog.Error("redis registry delete", "worker_id", workerID, "error", err)
	}
}

var _ WorkerRegistry = (*RedisRegistry)(nil)
