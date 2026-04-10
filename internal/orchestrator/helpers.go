package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"
)

// waitReady polls /readyz on the given address until it returns 200
// or the context is cancelled. Backend-agnostic — the caller passes an
// already-resolved address (cached on newServerInstance.Addr() at
// CreateInstance time), and waitReady only polls. The Docker-specific
// inspect-retry loop that used to live here moved into
// dockerServerFactory.CreateInstance.
func (o *Orchestrator) waitReady(ctx context.Context, addr string) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := o.checkReady(ctx, addr); err == nil {
				return nil
			}
		}
	}
}

// activate calls POST /api/v1/admin/activate on the new server using
// the activation token generated during Update.
func (o *Orchestrator) activate(ctx context.Context, addr string) error {
	url := "http://" + addr + "/api/v1/admin/activate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	if o.activationToken != "" {
		req.Header.Set("Authorization", "Bearer "+o.activationToken)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST activate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("activate returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// checkReady does a single /readyz check against the given address.
func (o *Orchestrator) checkReady(ctx context.Context, addr string) error {
	url := "http://" + addr + "/readyz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz returned %d", resp.StatusCode)
	}
	return nil
}

// generateActivationToken creates a cryptographically random token
// for authenticating the activate call between old and new servers.
func generateActivationToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based token if crypto/rand fails.
		return fmt.Sprintf("activation-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
