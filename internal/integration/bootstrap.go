package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// Bootstrap verifies OpenBao is configured correctly for blockyard.
// Checks:
//  1. OpenBao is reachable and unsealed (GET /v1/sys/health)
//  2. JWT auth method is enabled at the configured path
//  3. The "blockyard-user" role exists
//  4. KV v2 secrets engine is mounted at "secret/"
//  5. At least one attached policy uses per-user path scoping (warning only)
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

	// 5. Verify at least one attached policy uses per-user path scoping.
	if err := checkPolicyScoping(ctx, client, jwtAuthPath); err != nil {
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
	req.Header.Set("X-Vault-Token", client.adminTokenFunc())

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
	_, err := readRole(ctx, client, jwtAuthPath)
	return err
}

// roleResponse is the subset of the role read response we care about.
type roleResponse struct {
	Data struct {
		TokenPolicies []string `json:"token_policies"`
	} `json:"data"`
}

func readRole(ctx context.Context, client *Client, jwtAuthPath string) (*roleResponse, error) {
	url := fmt.Sprintf("%s/v1/auth/%s/role/blockyard-user", client.addr, jwtAuthPath)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("check role: %w", err)
	}
	req.Header.Set("X-Vault-Token", client.adminTokenFunc())

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("check role: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("blockyard-user role not found at auth/%s/role/blockyard-user", jwtAuthPath)
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("check role: status %d", resp.StatusCode)
	}

	var result roleResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("check role: decode: %w", err)
	}
	return &result, nil
}

func checkKVMount(ctx context.Context, client *Client) error {
	url := fmt.Sprintf("%s/v1/sys/mounts", client.addr)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("check kv mount: %w", err)
	}
	req.Header.Set("X-Vault-Token", client.adminTokenFunc())

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

// checkPolicyScoping reads the blockyard-user role's token_policies and
// verifies at least one policy uses identity-templated paths (e.g.
// {{identity.entity.aliases...}}). Without per-user scoping, all users
// share the same secret namespace — a serious misconfiguration.
func checkPolicyScoping(ctx context.Context, client *Client, jwtAuthPath string) error {
	role, err := readRole(ctx, client, jwtAuthPath)
	if err != nil {
		return fmt.Errorf("cannot read role for policy scoping check: %w", err)
	}

	for _, policyName := range role.Data.TokenPolicies {
		if policyName == "default" || policyName == "root" {
			continue
		}
		body, err := readPolicy(ctx, client, policyName)
		if err != nil {
			slog.Warn("bootstrap: cannot read policy for scoping check",
				"policy", policyName, "error", err)
			continue
		}
		if strings.Contains(body, "{{identity.") {
			return nil // found per-user scoping — all good
		}
	}

	return fmt.Errorf("no attached policy uses per-user path scoping " +
		"(expected {{identity.entity.aliases...}} template in policy paths) — " +
		"users may be able to read each other's secrets")
}

// policyResponse is the subset of the policy read response we need.
type policyResponse struct {
	Data struct {
		Policy string `json:"policy"` // raw HCL/JSON policy text
	} `json:"data"`
}

func readPolicy(ctx context.Context, client *Client, name string) (string, error) {
	url := fmt.Sprintf("%s/v1/sys/policy/%s", client.addr, name)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("read policy %s: %w", name, err)
	}
	req.Header.Set("X-Vault-Token", client.adminTokenFunc())

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("read policy %s: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("read policy %s: status %d", name, resp.StatusCode)
	}

	var result policyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("read policy %s: decode: %w", name, err)
	}
	return result.Data.Policy, nil
}
