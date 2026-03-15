package integration

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// TokenRenewer manages the lifecycle of a renewable vault token.
// It periodically renews the token at half its TTL, persists it to
// disk after each renewal, and tracks health for readyz checks.
type TokenRenewer struct {
	addr       string
	httpClient *http.Client
	token      atomic.Value // string
	tokenFile  string
	healthy    atomic.Bool
}

// NewTokenRenewer creates a TokenRenewer with the given initial token.
// The token is immediately marked as healthy.
func NewTokenRenewer(addr, initialToken, tokenFile string) *TokenRenewer {
	r := &TokenRenewer{
		addr:       addr,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		tokenFile:  tokenFile,
	}
	r.token.Store(initialToken)
	r.healthy.Store(true)
	return r
}

// Token returns the current vault token. Safe for concurrent use.
// This is intended as the adminTokenFunc callback for Client.
func (r *TokenRenewer) Token() string {
	return r.token.Load().(string)
}

// Healthy reports whether the vault token is valid and renewable.
func (r *TokenRenewer) Healthy() bool {
	return r.healthy.Load()
}

// Run starts the renewal loop. It renews the token at ttl/2 intervals.
// On failure, it retries with exponential backoff (1s → 60s cap).
// Blocks until ctx is cancelled.
func (r *TokenRenewer) Run(ctx context.Context, ttl time.Duration) {
	renewInterval := ttl / 2
	if renewInterval < 1*time.Second {
		renewInterval = 1 * time.Second
	}

	backoff := 1 * time.Second
	maxBackoff := 60 * time.Second
	timer := time.NewTimer(renewInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			newTTL, err := RenewSelf(ctx, r.httpClient, r.addr, r.Token())
			if err != nil {
				r.healthy.Store(false)
				slog.Warn("vault token renewal failed", "error", err, "retry_in", backoff)
				timer.Reset(backoff)
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}

			// Successful renewal — reset backoff, update state.
			backoff = 1 * time.Second
			r.healthy.Store(true)

			// Persist the token (it may have changed on renewal).
			if err := WriteTokenFile(r.tokenFile, r.Token()); err != nil {
				slog.Warn("failed to persist vault token", "error", err)
			}

			// Schedule next renewal at half the new TTL.
			renewInterval = newTTL / 2
			if renewInterval < 1*time.Second {
				renewInterval = 1 * time.Second
			}
			timer.Reset(renewInterval)
		}
	}
}
