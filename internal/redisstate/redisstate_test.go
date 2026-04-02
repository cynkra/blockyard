package redisstate

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/cynkra/blockyard/internal/config"
)

func TestClientKeyPrefix(t *testing.T) {
	mr := miniredis.RunT(t)
	c := TestClient(t, mr.Addr())

	if got := c.Key("session", "abc"); got != "test:session:abc" {
		t.Errorf("Key(session, abc) = %q, want %q", got, "test:session:abc")
	}
	if got := c.Key("worker", "xyz"); got != "test:worker:xyz" {
		t.Errorf("Key(worker, xyz) = %q, want %q", got, "test:worker:xyz")
	}
}

func TestClientPing(t *testing.T) {
	mr := miniredis.RunT(t)
	c := TestClient(t, mr.Addr())

	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
}

func TestClientPrefix(t *testing.T) {
	mr := miniredis.RunT(t)
	c := TestClient(t, mr.Addr())

	if got := c.Prefix(); got != "test:" {
		t.Errorf("Prefix() = %q, want %q", got, "test:")
	}
}

func TestNewFailsOnBadURL(t *testing.T) {
	_, err := New(context.Background(), &config.RedisConfig{URL: "not-a-url"})
	if err == nil {
		t.Fatal("expected error for bad URL")
	}
}

func TestNewFailsOnUnreachable(t *testing.T) {
	_, err := New(context.Background(), &config.RedisConfig{
		URL:       "redis://127.0.0.1:1/0", // port 1 — unreachable
		KeyPrefix: "test:",
	})
	if err == nil {
		t.Fatal("expected error for unreachable Redis")
	}
}
