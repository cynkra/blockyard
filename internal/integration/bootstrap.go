package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Bootstrap verifies OpenBao is configured correctly for blockyard.
// Checks:
//  1. OpenBao is reachable and unsealed (GET /v1/sys/health)
//  2. JWT auth method is enabled at the configured path
//  3. The "blockyard-user" role exists
//  4. KV v2 secrets engine is mounted at "secret/"
//
// Returns nil if all checks pass. Returns an error describing the
// first failure. The caller decides whether to treat this as fatal.
func Bootstrap(ctx context.Context, client *Client, jwtAuthPath string) error {
	// 1. Health check
	if err := client.Health(ctx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	// 2. Check JWT auth method is enabled
	if err := checkJWTAuth(ctx, client, jwtAuthPath); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	// 3. Check blockyard-user role exists
	if err := checkRole(ctx, client, jwtAuthPath); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	// 4. Check KV v2 is mounted at secret/
	if err := checkKVMount(ctx, client); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	return nil
}

func checkJWTAuth(ctx context.Context, client *Client, jwtAuthPath string) error {
	url := fmt.Sprintf("%s/v1/sys/auth", client.addr)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("check jwt auth: %w", err)
	}
	req.Header.Set("X-Vault-Token", client.adminToken)

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("check jwt auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("check jwt auth: status %d", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("check jwt auth: decode: %w", err)
	}

	// Auth methods are keyed as "path/" (with trailing slash).
	key := jwtAuthPath + "/"
	if _, ok := result[key]; !ok {
		return fmt.Errorf("JWT auth method not enabled at %q", jwtAuthPath)
	}
	return nil
}

func checkRole(ctx context.Context, client *Client, jwtAuthPath string) error {
	url := fmt.Sprintf("%s/v1/auth/%s/role/blockyard-user", client.addr, jwtAuthPath)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("check role: %w", err)
	}
	req.Header.Set("X-Vault-Token", client.adminToken)

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("check role: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("blockyard-user role not found at auth/%s/role/blockyard-user", jwtAuthPath)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("check role: status %d", resp.StatusCode)
	}
	return nil
}

func checkKVMount(ctx context.Context, client *Client) error {
	url := fmt.Sprintf("%s/v1/sys/mounts", client.addr)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("check kv mount: %w", err)
	}
	req.Header.Set("X-Vault-Token", client.adminToken)

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("check kv mount: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("check kv mount: status %d", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("check kv mount: decode: %w", err)
	}

	if _, ok := result["secret/"]; !ok {
		return fmt.Errorf("KV v2 secrets engine not mounted at secret/")
	}
	return nil
}
