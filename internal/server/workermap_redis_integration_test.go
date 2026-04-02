//go:build redis_test

package server

import (
	"os"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/redisstate"
)

func testAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR not set")
	}
	return addr
}

func TestRealRedisWorkerMapRoundTrip(t *testing.T) {
	client := redisstate.TestClient(t, testAddr(t))
	m := NewRedisWorkerMap(client, "test-host")

	now := time.Now().Truncate(time.Second)
	m.Set("w1", ActiveWorker{
		AppID:     "app1",
		BundleID:  "b1",
		StartedAt: now,
	})

	w, ok := m.Get("w1")
	if !ok {
		t.Fatal("expected worker to exist")
	}
	if w.AppID != "app1" || w.BundleID != "b1" {
		t.Errorf("unexpected worker: %+v", w)
	}

	m.Delete("w1")
	if _, ok := m.Get("w1"); ok {
		t.Error("expected worker to be deleted")
	}
}
