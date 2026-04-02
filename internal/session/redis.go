package session

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// RedisStore implements session.Store using Redis hashes.
//
// Key schema:
//
//	{prefix}session:{sessionID}  ->  hash {worker_id, user_sub, last_access}
//
// Each session key has a TTL equal to idleTTL. Touch refreshes it.
// SweepIdle is a no-op — Redis TTL expiry handles idle cleanup.
type RedisStore struct {
	client  *redisstate.Client
	idleTTL time.Duration
}

func NewRedisStore(client *redisstate.Client, idleTTL time.Duration) *RedisStore {
	return &RedisStore{client: client, idleTTL: idleTTL}
}

func (s *RedisStore) sessionKey(sessionID string) string {
	return s.client.Key("session", sessionID)
}

func (s *RedisStore) Get(sessionID string) (Entry, bool) {
	ctx := context.Background()
	vals, err := s.client.Redis().HGetAll(ctx, s.sessionKey(sessionID)).Result()
	if err != nil {
		slog.Error("redis session get", "session_id", sessionID, "error", err)
		return Entry{}, false
	}
	if len(vals) == 0 {
		return Entry{}, false
	}
	return parseSessionHash(vals), true
}

func (s *RedisStore) Set(sessionID string, entry Entry) {
	ctx := context.Background()
	key := s.sessionKey(sessionID)

	pipe := s.client.Redis().Pipeline()
	pipe.HSet(ctx, key,
		"worker_id", entry.WorkerID,
		"user_sub", entry.UserSub,
		"last_access", strconv.FormatInt(entry.LastAccess.Unix(), 10),
	)
	if s.idleTTL > 0 {
		pipe.Expire(ctx, key, s.idleTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Error("redis session set", "session_id", sessionID, "error", err)
	}
}

func (s *RedisStore) Touch(sessionID string) bool {
	ctx := context.Background()
	key := s.sessionKey(sessionID)

	exists, err := s.client.Redis().Exists(ctx, key).Result()
	if err != nil {
		slog.Error("redis session touch exists", "session_id", sessionID, "error", err)
		return false
	}
	if exists == 0 {
		return false
	}

	pipe := s.client.Redis().Pipeline()
	pipe.HSet(ctx, key, "last_access", strconv.FormatInt(time.Now().Unix(), 10))
	if s.idleTTL > 0 {
		pipe.Expire(ctx, key, s.idleTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Error("redis session touch", "session_id", sessionID, "error", err)
		return false
	}
	return true
}

func (s *RedisStore) Delete(sessionID string) {
	ctx := context.Background()
	if err := s.client.Redis().Del(ctx, s.sessionKey(sessionID)).Err(); err != nil {
		slog.Error("redis session delete", "session_id", sessionID, "error", err)
	}
}

// deleteByWorkerScript scans all session keys and deletes those belonging to a worker.
var deleteByWorkerScript = redis.NewScript(`
local prefix = KEYS[1]
local worker_id = ARGV[1]
local cursor = "0"
local deleted = 0
repeat
    local result = redis.call("SCAN", cursor, "MATCH", prefix .. "session:*", "COUNT", 100)
    cursor = result[1]
    for _, key in ipairs(result[2]) do
        if redis.call("HGET", key, "worker_id") == worker_id then
            redis.call("DEL", key)
            deleted = deleted + 1
        end
    end
until cursor == "0"
return deleted
`)

func (s *RedisStore) DeleteByWorker(workerID string) int {
	ctx := context.Background()
	n, err := deleteByWorkerScript.Run(ctx, s.client.Redis(),
		[]string{s.client.Prefix()}, workerID).Int()
	if err != nil {
		slog.Error("redis session delete by worker", "worker_id", workerID, "error", err)
		return 0
	}
	return n
}

// countForWorkerScript counts sessions belonging to a specific worker.
var countForWorkerScript = redis.NewScript(`
local prefix = KEYS[1]
local worker_id = ARGV[1]
local cursor = "0"
local count = 0
repeat
    local result = redis.call("SCAN", cursor, "MATCH", prefix .. "session:*", "COUNT", 100)
    cursor = result[1]
    for _, key in ipairs(result[2]) do
        if redis.call("HGET", key, "worker_id") == worker_id then
            count = count + 1
        end
    end
until cursor == "0"
return count
`)

func (s *RedisStore) CountForWorker(workerID string) int {
	ctx := context.Background()
	n, err := countForWorkerScript.Run(ctx, s.client.Redis(),
		[]string{s.client.Prefix()}, workerID).Int()
	if err != nil {
		slog.Error("redis session count for worker", "worker_id", workerID, "error", err)
		return 0
	}
	return n
}

// countForWorkersScript counts sessions belonging to any of the given worker IDs.
var countForWorkersScript = redis.NewScript(`
local prefix = KEYS[1]
local set = {}
for i = 1, #ARGV do
    set[ARGV[i]] = true
end
local cursor = "0"
local count = 0
repeat
    local result = redis.call("SCAN", cursor, "MATCH", prefix .. "session:*", "COUNT", 100)
    cursor = result[1]
    for _, key in ipairs(result[2]) do
        local wid = redis.call("HGET", key, "worker_id")
        if set[wid] then
            count = count + 1
        end
    end
until cursor == "0"
return count
`)

func (s *RedisStore) CountForWorkers(workerIDs []string) int {
	if len(workerIDs) == 0 {
		return 0
	}
	ctx := context.Background()
	args := make([]any, len(workerIDs))
	for i, id := range workerIDs {
		args[i] = id
	}
	n, err := countForWorkersScript.Run(ctx, s.client.Redis(),
		[]string{s.client.Prefix()}, args...).Int()
	if err != nil {
		slog.Error("redis session count for workers", "error", err)
		return 0
	}
	return n
}

// rerouteWorkerScript reassigns all sessions from one worker to another.
var rerouteWorkerScript = redis.NewScript(`
local prefix = KEYS[1]
local old_worker = ARGV[1]
local new_worker = ARGV[2]
local cursor = "0"
local count = 0
repeat
    local result = redis.call("SCAN", cursor, "MATCH", prefix .. "session:*", "COUNT", 100)
    cursor = result[1]
    for _, key in ipairs(result[2]) do
        if redis.call("HGET", key, "worker_id") == old_worker then
            redis.call("HSET", key, "worker_id", new_worker)
            count = count + 1
        end
    end
until cursor == "0"
return count
`)

func (s *RedisStore) RerouteWorker(oldWorkerID, newWorkerID string) int {
	ctx := context.Background()
	n, err := rerouteWorkerScript.Run(ctx, s.client.Redis(),
		[]string{s.client.Prefix()}, oldWorkerID, newWorkerID).Int()
	if err != nil {
		slog.Error("redis session reroute worker",
			"old_worker_id", oldWorkerID, "new_worker_id", newWorkerID, "error", err)
		return 0
	}
	return n
}

func (s *RedisStore) EntriesForWorker(workerID string) map[string]Entry {
	ctx := context.Background()
	prefix := s.client.Prefix()
	pattern := prefix + "session:*"
	prefixLen := len(prefix) + len("session:")

	result := make(map[string]Entry)
	var cursor uint64
	for {
		keys, next, err := s.client.Redis().Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			slog.Error("redis session entries scan", "worker_id", workerID, "error", err)
			return result
		}

		// Pipeline HGETALL for all keys in this batch.
		if len(keys) > 0 {
			pipe := s.client.Redis().Pipeline()
			cmds := make([]*redis.MapStringStringCmd, len(keys))
			for i, key := range keys {
				cmds[i] = pipe.HGetAll(ctx, key)
			}
			if _, err := pipe.Exec(ctx); err != nil {
				slog.Error("redis session entries pipeline", "worker_id", workerID, "error", err)
				return result
			}
			for i, cmd := range cmds {
				vals, err := cmd.Result()
				if err != nil || len(vals) == 0 {
					continue
				}
				if vals["worker_id"] == workerID {
					sessionID := keys[i][prefixLen:]
					result[sessionID] = parseSessionHash(vals)
				}
			}
		}

		cursor = next
		if cursor == 0 {
			break
		}
	}
	return result
}

// SweepIdle is a no-op for Redis — TTL-based expiry handles idle cleanup.
func (s *RedisStore) SweepIdle(_ time.Duration) int {
	return 0
}

// parseSessionHash converts a Redis hash map to a session Entry.
func parseSessionHash(vals map[string]string) Entry {
	var e Entry
	e.WorkerID = vals["worker_id"]
	e.UserSub = vals["user_sub"]
	if ts, err := strconv.ParseInt(vals["last_access"], 10, 64); err == nil {
		e.LastAccess = time.Unix(ts, 0)
	}
	return e
}

var _ Store = (*RedisStore)(nil)
