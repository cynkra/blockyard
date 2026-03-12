package integration

import (
	"testing"
	"time"
)

func TestVaultTokenCache_HitAndMiss(t *testing.T) {
	c := NewVaultTokenCache()

	// Miss on empty cache.
	if _, ok := c.Get("user-1"); ok {
		t.Fatal("expected cache miss for empty cache")
	}

	// Set and hit.
	c.Set("user-1", "token-abc", 1*time.Hour)
	token, ok := c.Get("user-1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if token != "token-abc" {
		t.Errorf("token = %q, want token-abc", token)
	}

	// Miss for different user.
	if _, ok := c.Get("user-2"); ok {
		t.Fatal("expected cache miss for different user")
	}
}

func TestVaultTokenCache_Expiry(t *testing.T) {
	c := NewVaultTokenCache()

	// Set with very short TTL that's within the renewal buffer.
	c.Set("user-1", "token-expired", 10*time.Second)

	// Should miss because 10s < 30s renewal buffer.
	if _, ok := c.Get("user-1"); ok {
		t.Fatal("expected cache miss for token within renewal buffer")
	}
}

func TestVaultTokenCache_ValidTTL(t *testing.T) {
	c := NewVaultTokenCache()

	// Set with TTL well beyond renewal buffer.
	c.Set("user-1", "token-valid", 5*time.Minute)

	token, ok := c.Get("user-1")
	if !ok {
		t.Fatal("expected cache hit for valid token")
	}
	if token != "token-valid" {
		t.Errorf("token = %q, want token-valid", token)
	}
}

func TestVaultTokenCache_Delete(t *testing.T) {
	c := NewVaultTokenCache()

	c.Set("user-1", "token-abc", 1*time.Hour)
	c.Delete("user-1")

	if _, ok := c.Get("user-1"); ok {
		t.Fatal("expected cache miss after delete")
	}
}

func TestVaultTokenCache_Sweep(t *testing.T) {
	c := NewVaultTokenCache()

	// One valid, two expired (TTL=0 means already expired).
	c.Set("valid", "token-v", 1*time.Hour)
	c.Set("expired-1", "token-e1", 0)
	c.Set("expired-2", "token-e2", 0)

	removed := c.Sweep()
	if removed != 2 {
		t.Errorf("Sweep removed %d, want 2", removed)
	}

	// Valid token should still be there.
	if _, ok := c.Get("valid"); !ok {
		t.Error("expected valid token to survive sweep")
	}

	// Expired tokens should be gone from the map (not just from Get).
	c.mu.RLock()
	if _, ok := c.tokens["expired-1"]; ok {
		t.Error("expected expired-1 to be removed from map")
	}
	if _, ok := c.tokens["expired-2"]; ok {
		t.Error("expected expired-2 to be removed from map")
	}
	c.mu.RUnlock()
}

func TestVaultTokenCache_SweepEmpty(t *testing.T) {
	c := NewVaultTokenCache()
	if removed := c.Sweep(); removed != 0 {
		t.Errorf("Sweep on empty cache removed %d, want 0", removed)
	}
}

func TestVaultTokenCache_Overwrite(t *testing.T) {
	c := NewVaultTokenCache()

	c.Set("user-1", "token-old", 1*time.Hour)
	c.Set("user-1", "token-new", 1*time.Hour)

	token, ok := c.Get("user-1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if token != "token-new" {
		t.Errorf("token = %q, want token-new", token)
	}
}
