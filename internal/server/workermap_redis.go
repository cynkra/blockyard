package server

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/cynkra/blockyard/internal/redisstate"
)

// RedisWorkerMap implements WorkerMap using Redis hashes.
//
// Key schema:
//
//	{prefix}worker:{workerID}  ->  hash {app_id, bundle_id, draining, idle_since, started_at, server_id}
//
// No TTL — workers are explicitly deleted on eviction.
type RedisWorkerMap struct {
	client   *redisstate.Client
	serverID string
}

func NewRedisWorkerMap(client *redisstate.Client, serverID string) *RedisWorkerMap {
	return &RedisWorkerMap{client: client, serverID: serverID}
}

func (m *RedisWorkerMap) workerKey(workerID string) string {
	return m.client.Key("worker", workerID)
}

func (m *RedisWorkerMap) Get(id string) (ActiveWorker, bool) {
	ctx := context.Background()
	vals, err := m.client.Redis().HGetAll(ctx, m.workerKey(id)).Result()
	if err != nil {
		slog.Error("redis worker get", "worker_id", id, "error", err)
		return ActiveWorker{}, false
	}
	if len(vals) == 0 {
		return ActiveWorker{}, false
	}
	return parseWorkerHash(vals), true
}

func (m *RedisWorkerMap) Set(id string, w ActiveWorker) {
	ctx := context.Background()
	idleSince := "0"
	if !w.IdleSince.IsZero() {
		idleSince = strconv.FormatInt(w.IdleSince.Unix(), 10)
	}
	if err := m.client.Redis().HSet(ctx, m.workerKey(id),
		"app_id", w.AppID,
		"bundle_id", w.BundleID,
		"draining", boolToStr(w.Draining),
		"idle_since", idleSince,
		"started_at", strconv.FormatInt(w.StartedAt.Unix(), 10),
		"server_id", m.serverID,
	).Err(); err != nil {
		slog.Error("redis worker set", "worker_id", id, "error", err)
	}
}

func (m *RedisWorkerMap) Delete(id string) {
	ctx := context.Background()
	if err := m.client.Redis().Del(ctx, m.workerKey(id)).Err(); err != nil {
		slog.Error("redis worker delete", "worker_id", id, "error", err)
	}
}

// countScript counts all worker keys.
var countScript = redis.NewScript(`
local prefix = KEYS[1]
local cursor = "0"
local count = 0
repeat
    local result = redis.call("SCAN", cursor, "MATCH", prefix .. "worker:*", "COUNT", 100)
    cursor = result[1]
    count = count + #result[2]
until cursor == "0"
return count
`)

func (m *RedisWorkerMap) Count() int {
	ctx := context.Background()
	n, err := countScript.Run(ctx, m.client.Redis(),
		[]string{m.client.Prefix()}).Int()
	if err != nil {
		slog.Error("redis worker count", "error", err)
		return 0
	}
	return n
}

// countForAppScript counts workers for a specific app.
var countForAppScript = redis.NewScript(`
local prefix = KEYS[1]
local app_id = ARGV[1]
local cursor = "0"
local count = 0
repeat
    local result = redis.call("SCAN", cursor, "MATCH", prefix .. "worker:*", "COUNT", 100)
    cursor = result[1]
    for _, key in ipairs(result[2]) do
        if redis.call("HGET", key, "app_id") == app_id then
            count = count + 1
        end
    end
until cursor == "0"
return count
`)

func (m *RedisWorkerMap) CountForApp(appID string) int {
	ctx := context.Background()
	n, err := countForAppScript.Run(ctx, m.client.Redis(),
		[]string{m.client.Prefix()}, appID).Int()
	if err != nil {
		slog.Error("redis worker count for app", "app_id", appID, "error", err)
		return 0
	}
	return n
}

func (m *RedisWorkerMap) All() []string {
	ctx := context.Background()
	prefix := m.client.Prefix()
	pattern := prefix + "worker:*"
	prefixLen := len(prefix) + len("worker:")

	var ids []string
	var cursor uint64
	for {
		keys, next, err := m.client.Redis().Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			slog.Error("redis worker all scan", "error", err)
			return ids
		}
		for _, key := range keys {
			ids = append(ids, key[prefixLen:])
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return ids
}

func (m *RedisWorkerMap) ForApp(appID string) []string {
	ctx := context.Background()
	prefix := m.client.Prefix()
	pattern := prefix + "worker:*"
	prefixLen := len(prefix) + len("worker:")

	var ids []string
	var cursor uint64
	for {
		keys, next, err := m.client.Redis().Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			slog.Error("redis worker for app scan", "app_id", appID, "error", err)
			return ids
		}
		if len(keys) > 0 {
			pipe := m.client.Redis().Pipeline()
			cmds := make([]*redis.StringCmd, len(keys))
			for i, key := range keys {
				cmds[i] = pipe.HGet(ctx, key, "app_id")
			}
			pipe.Exec(ctx) //nolint:errcheck
			for i, cmd := range cmds {
				if v, err := cmd.Result(); err == nil && v == appID {
					ids = append(ids, keys[i][prefixLen:])
				}
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return ids
}

func (m *RedisWorkerMap) ForAppAvailable(appID string) []string {
	ctx := context.Background()
	prefix := m.client.Prefix()
	pattern := prefix + "worker:*"
	prefixLen := len(prefix) + len("worker:")

	var ids []string
	var cursor uint64
	for {
		keys, next, err := m.client.Redis().Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			slog.Error("redis worker for app available scan", "app_id", appID, "error", err)
			return ids
		}
		if len(keys) > 0 {
			pipe := m.client.Redis().Pipeline()
			cmds := make([]*redis.MapStringStringCmd, len(keys))
			for i, key := range keys {
				cmds[i] = pipe.HGetAll(ctx, key)
			}
			pipe.Exec(ctx) //nolint:errcheck
			for i, cmd := range cmds {
				vals, err := cmd.Result()
				if err != nil || len(vals) == 0 {
					continue
				}
				if vals["app_id"] == appID && vals["draining"] != "1" {
					ids = append(ids, keys[i][prefixLen:])
				}
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return ids
}

// markDrainingScript sets draining=1 on all workers for an app and returns their IDs.
var markDrainingScript = redis.NewScript(`
local prefix = KEYS[1]
local app_id = ARGV[1]
local cursor = "0"
local ids = {}
repeat
    local result = redis.call("SCAN", cursor, "MATCH", prefix .. "worker:*", "COUNT", 100)
    cursor = result[1]
    for _, key in ipairs(result[2]) do
        if redis.call("HGET", key, "app_id") == app_id then
            redis.call("HSET", key, "draining", "1")
            ids[#ids + 1] = string.sub(key, #prefix + #"worker:" + 1)
        end
    end
until cursor == "0"
return ids
`)

func (m *RedisWorkerMap) MarkDraining(appID string) []string {
	ctx := context.Background()
	result, err := markDrainingScript.Run(ctx, m.client.Redis(),
		[]string{m.client.Prefix()}, appID).StringSlice()
	if err != nil {
		// redis.Nil means no matches — return empty slice.
		if err == redis.Nil {
			return nil
		}
		slog.Error("redis worker mark draining", "app_id", appID, "error", err)
		return nil
	}
	return result
}

// setFieldIfExistsScript guards single-field mutations against ghost entries.
var setFieldIfExistsScript = redis.NewScript(`
local key = KEYS[1]
if redis.call("EXISTS", key) == 1 then
    redis.call("HSET", key, ARGV[1], ARGV[2])
end
return 0
`)

func (m *RedisWorkerMap) SetDraining(workerID string) {
	ctx := context.Background()
	if err := setFieldIfExistsScript.Run(ctx, m.client.Redis(),
		[]string{m.workerKey(workerID)}, "draining", "1").Err(); err != nil {
		slog.Error("redis worker set draining", "worker_id", workerID, "error", err)
	}
}

func (m *RedisWorkerMap) ClearDraining(workerID string) {
	ctx := context.Background()
	if err := setFieldIfExistsScript.Run(ctx, m.client.Redis(),
		[]string{m.workerKey(workerID)}, "draining", "0").Err(); err != nil {
		slog.Error("redis worker clear draining", "worker_id", workerID, "error", err)
	}
}

func (m *RedisWorkerMap) SetIdleSince(workerID string, t time.Time) {
	ctx := context.Background()
	if err := setFieldIfExistsScript.Run(ctx, m.client.Redis(),
		[]string{m.workerKey(workerID)}, "idle_since", strconv.FormatInt(t.Unix(), 10)).Err(); err != nil {
		slog.Error("redis worker set idle since", "worker_id", workerID, "error", err)
	}
}

// setIdleSinceIfZeroScript sets idle_since only if it's currently "0".
var setIdleSinceIfZeroScript = redis.NewScript(`
local key = KEYS[1]
if redis.call("EXISTS", key) == 1 then
    local cur = redis.call("HGET", key, "idle_since")
    if cur == "0" then
        redis.call("HSET", key, "idle_since", ARGV[1])
    end
end
return 0
`)

func (m *RedisWorkerMap) SetIdleSinceIfZero(workerID string, t time.Time) {
	ctx := context.Background()
	if err := setIdleSinceIfZeroScript.Run(ctx, m.client.Redis(),
		[]string{m.workerKey(workerID)}, strconv.FormatInt(t.Unix(), 10)).Err(); err != nil {
		slog.Error("redis worker set idle since if zero", "worker_id", workerID, "error", err)
	}
}

// clearIdleSinceScript resets idle_since to "0" and returns whether it was non-zero.
var clearIdleSinceScript = redis.NewScript(`
local key = KEYS[1]
if redis.call("EXISTS", key) == 1 then
    local cur = redis.call("HGET", key, "idle_since")
    redis.call("HSET", key, "idle_since", "0")
    if cur ~= "0" then
        return 1
    end
end
return 0
`)

func (m *RedisWorkerMap) ClearIdleSince(workerID string) bool {
	ctx := context.Background()
	n, err := clearIdleSinceScript.Run(ctx, m.client.Redis(),
		[]string{m.workerKey(workerID)}).Int()
	if err != nil {
		slog.Error("redis worker clear idle since", "worker_id", workerID, "error", err)
		return false
	}
	return n == 1
}

// idleWorkersScript returns worker IDs idle longer than timeout, excluding draining.
var idleWorkersScript = redis.NewScript(`
local prefix = KEYS[1]
local cutoff = tonumber(ARGV[1])
local cursor = "0"
local ids = {}
repeat
    local result = redis.call("SCAN", cursor, "MATCH", prefix .. "worker:*", "COUNT", 100)
    cursor = result[1]
    for _, key in ipairs(result[2]) do
        local idle = redis.call("HGET", key, "idle_since")
        local draining = redis.call("HGET", key, "draining")
        if idle ~= "0" and draining ~= "1" then
            if tonumber(idle) <= cutoff then
                ids[#ids + 1] = string.sub(key, #prefix + #"worker:" + 1)
            end
        end
    end
until cursor == "0"
return ids
`)

func (m *RedisWorkerMap) IdleWorkers(timeout time.Duration) []string {
	ctx := context.Background()
	cutoff := time.Now().Add(-timeout).Unix()
	result, err := idleWorkersScript.Run(ctx, m.client.Redis(),
		[]string{m.client.Prefix()}, cutoff).StringSlice()
	if err != nil {
		if err == redis.Nil {
			return nil
		}
		slog.Error("redis worker idle workers", "error", err)
		return nil
	}
	return result
}

// appIDsScript collects unique app_id values from all workers.
var appIDsScript = redis.NewScript(`
local prefix = KEYS[1]
local cursor = "0"
local seen = {}
local ids = {}
repeat
    local result = redis.call("SCAN", cursor, "MATCH", prefix .. "worker:*", "COUNT", 100)
    cursor = result[1]
    for _, key in ipairs(result[2]) do
        local app_id = redis.call("HGET", key, "app_id")
        if not seen[app_id] then
            seen[app_id] = true
            ids[#ids + 1] = app_id
        end
    end
until cursor == "0"
return ids
`)

func (m *RedisWorkerMap) AppIDs() []string {
	ctx := context.Background()
	result, err := appIDsScript.Run(ctx, m.client.Redis(),
		[]string{m.client.Prefix()}).StringSlice()
	if err != nil {
		if err == redis.Nil {
			return nil
		}
		slog.Error("redis worker app ids", "error", err)
		return nil
	}
	return result
}

// isDrainingScript checks if any worker with the given app_id is draining.
var isDrainingScript = redis.NewScript(`
local prefix = KEYS[1]
local app_id = ARGV[1]
local cursor = "0"
repeat
    local result = redis.call("SCAN", cursor, "MATCH", prefix .. "worker:*", "COUNT", 100)
    cursor = result[1]
    for _, key in ipairs(result[2]) do
        if redis.call("HGET", key, "app_id") == app_id then
            if redis.call("HGET", key, "draining") == "1" then
                return 1
            end
        end
    end
until cursor == "0"
return 0
`)

func (m *RedisWorkerMap) IsDraining(appID string) bool {
	ctx := context.Background()
	n, err := isDrainingScript.Run(ctx, m.client.Redis(),
		[]string{m.client.Prefix()}, appID).Int()
	if err != nil {
		slog.Error("redis worker is draining", "app_id", appID, "error", err)
		return false
	}
	return n == 1
}

// parseWorkerHash converts a Redis hash map to an ActiveWorker.
func parseWorkerHash(vals map[string]string) ActiveWorker {
	var w ActiveWorker
	w.AppID = vals["app_id"]
	w.BundleID = vals["bundle_id"]
	w.Draining = vals["draining"] == "1"
	if ts, err := strconv.ParseInt(vals["idle_since"], 10, 64); err == nil && ts != 0 {
		w.IdleSince = time.Unix(ts, 0)
	}
	if ts, err := strconv.ParseInt(vals["started_at"], 10, 64); err == nil {
		w.StartedAt = time.Unix(ts, 0)
	}
	return w
}

func boolToStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

var _ WorkerMap = (*RedisWorkerMap)(nil)
