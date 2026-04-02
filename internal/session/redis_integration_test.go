//go:build redis_test

package session

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

func TestRealRedisSessionStoreRoundTrip(t *testing.T) {
	client := redisstate.TestClient(t, testAddr(t))
	s := NewRedisStore(client, time.Hour)

	now := time.Now().Truncate(time.Second)
	s.Set("sess-1", Entry{WorkerID: "w1", UserSub: "user-a", LastAccess: now})

	e, ok := s.Get("sess-1")
	if !ok {
		t.Fatal("expected session to exist")
	}
	if e.WorkerID != "w1" || e.UserSub != "user-a" {
		t.Errorf("unexpected entry: %+v", e)
	}

	s.Delete("sess-1")
	if _, ok := s.Get("sess-1"); ok {
		t.Error("expected session to be deleted")
	}
}
