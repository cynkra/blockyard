package redisstate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/cynkra/blockyard/internal/config"
)

// Client wraps a Redis connection with a key prefix.
type Client struct {
	rdb    *redis.Client
	prefix string
}

// New parses the config, connects, and verifies with a PING.
func New(ctx context.Context, cfg *config.RedisConfig) (*Client, error) {
	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	rdb := redis.NewClient(opts)

	if opts.Password == "" {
		slog.Warn("redis connection has no authentication; consider enabling AUTH for production")
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return &Client{rdb: rdb, prefix: cfg.KeyPrefix}, nil
}

// Close closes the underlying Redis connection.
func (c *Client) Close() error {
	return c.rdb.Close()
}

// Ping checks Redis connectivity.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Key returns the prefixed key for the given components.
// Key("session", "abc") -> "blockyard:session:abc"
func (c *Client) Key(parts ...string) string {
	k := c.prefix
	for i, p := range parts {
		if i > 0 {
			k += ":"
		}
		k += p
	}
	return k
}

// Redis returns the underlying go-redis client for direct use
// by store implementations.
func (c *Client) Redis() *redis.Client {
	return c.rdb
}

// Prefix returns the configured key prefix.
func (c *Client) Prefix() string {
	return c.prefix
}
