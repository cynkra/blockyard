package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenRenewerToken(t *testing.T) {
	r := NewTokenRenewer("http://localhost", "initial-token", "")
	if r.Token() != "initial-token" {
		t.Errorf("Token() = %q", r.Token())
	}
	if !r.Healthy() {
		t.Error("expected healthy after creation")
	}
}

func TestTokenRenewerRun(t *testing.T) {
	var renewCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		renewCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "hvs.renewed",
				"lease_duration": 2, // 2 seconds — renewal at 1s
			},
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, ".vault-token")

	r := NewTokenRenewer(srv.URL, "hvs.initial", tokenFile)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go r.Run(ctx, 2*time.Second) // ttl=2s → renew at 1s

	// Wait for at least one renewal.
	time.Sleep(1500 * time.Millisecond)

	if renewCount.Load() < 1 {
		t.Error("expected at least one renewal")
	}
	if !r.Healthy() {
		t.Error("expected healthy after successful renewal")
	}

	// Check token was persisted.
	persisted, err := ReadTokenFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if persisted != "hvs.initial" {
		t.Errorf("persisted token = %q", persisted)
	}
}

func TestTokenRenewerBackoff(t *testing.T) {
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(403) // Always fail
	}))
	defer srv.Close()

	dir := t.TempDir()
	r := NewTokenRenewer(srv.URL, "hvs.will-fail", filepath.Join(dir, ".vault-token"))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go r.Run(ctx, 2*time.Second) // ttl=2s → first renewal at 1s

	time.Sleep(2500 * time.Millisecond)

	if r.Healthy() {
		t.Error("expected unhealthy after failed renewals")
	}
	if callCount.Load() < 2 {
		t.Errorf("expected at least 2 retry attempts, got %d", callCount.Load())
	}
}

func TestTokenRenewerContextCancel(t *testing.T) {
	r := NewTokenRenewer("http://localhost", "hvs.token", "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx, 10*time.Hour)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// OK — Run exited
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancel")
	}
}
