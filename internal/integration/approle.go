package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
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

// InitAppRole authenticates to vault using AppRole. It first tries
// a persisted token (renew-self), then falls back to AppRole login with
// secret_id from the environment.
func InitAppRole(ctx context.Context, addr, roleID, tokenFile string) (token string, ttl time.Duration, err error) {
	httpClient := &http.Client{}

	// 1. Try persisted token.
	persisted, err := ReadTokenFile(tokenFile)
	if err != nil {
		slog.Warn("failed to read persisted vault token", "error", err)
	}
	if persisted != "" {
		renewTTL, err := RenewSelf(ctx, httpClient, addr, persisted)
		if err == nil {
			slog.Info("reusing persisted vault token")
			return persisted, renewTTL, nil
		}
		slog.Warn("persisted vault token renewal failed, trying AppRole login", "error", err)
	}

	// 2. AppRole login with secret_id from env.
	// (BLOCKYARD_OPENBAO_SECRET_ID is renamed to BLOCKYARD_VAULT_SECRET_ID by
	// the deprecation shim in config.Load before this runs.)
	secretID := os.Getenv("BLOCKYARD_VAULT_SECRET_ID")
	if secretID == "" {
		return "", 0, fmt.Errorf("vault bootstrap required: set BLOCKYARD_VAULT_SECRET_ID")
	}

	token, ttl, err = AppRoleLogin(ctx, httpClient, addr, roleID, secretID)
	if err != nil {
		return "", 0, fmt.Errorf("AppRole login failed: %w", err)
	}

	// Persist the token for restart reuse.
	if writeErr := WriteTokenFile(tokenFile, token); writeErr != nil {
		slog.Warn("failed to persist vault token", "error", writeErr)
	}

	slog.Info("vault AppRole authentication successful")
	return token, ttl, nil
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
