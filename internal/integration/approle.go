package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// appRoleLoginResponse is the relevant subset of the AppRole login response.
type appRoleLoginResponse struct {
	Auth struct {
		ClientToken   string `json:"client_token"`
		LeaseDuration int    `json:"lease_duration"`
	} `json:"auth"`
	Errors []string `json:"errors"`
}

// AppRoleLogin authenticates to OpenBao using AppRole credentials.
// Returns the client token and its TTL.
func AppRoleLogin(ctx context.Context, httpClient *http.Client, addr, roleID, secretID string) (token string, ttl time.Duration, err error) {
	body := fmt.Sprintf(`{"role_id":%q,"secret_id":%q}`, roleID, secretID)
	url := strings.TrimRight(addr, "/") + "/v1/auth/approle/login"

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("approle login: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("approle login: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", 0, fmt.Errorf("approle login: status %d", resp.StatusCode)
	}

	var result appRoleLoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", 0, fmt.Errorf("approle login: decode: %w", err)
	}

	if result.Auth.ClientToken == "" {
		return "", 0, fmt.Errorf("approle login: empty client_token")
	}

	return result.Auth.ClientToken, time.Duration(result.Auth.LeaseDuration) * time.Second, nil
}

// tokenRenewResponse is the relevant subset of the token renew-self response.
type tokenRenewResponse struct {
	Auth struct {
		ClientToken   string `json:"client_token"`
		LeaseDuration int    `json:"lease_duration"`
	} `json:"auth"`
}

// RenewSelf renews the given vault token. Returns the new TTL.
func RenewSelf(ctx context.Context, httpClient *http.Client, addr, token string) (time.Duration, error) {
	url := strings.TrimRight(addr, "/") + "/v1/auth/token/renew-self"

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return 0, fmt.Errorf("renew-self: %w", err)
	}
	req.Header.Set("X-Vault-Token", token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("renew-self: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return 0, fmt.Errorf("renew-self: status %d", resp.StatusCode)
	}

	var result tokenRenewResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("renew-self: decode: %w", err)
	}

	return time.Duration(result.Auth.LeaseDuration) * time.Second, nil
}
