package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotFound is returned by KVRead when the secret path does not
// exist in vault. Callers can use errors.Is to distinguish this
// from transient failures.
var ErrNotFound = errors.New("secret not found")

// Client is a lightweight HTTP client for OpenBao's REST API.
// It targets only the endpoints blockyard needs: JWT auth login,
// KV v2 read/write, and sys/health.
type Client struct {
	addr       string
	admin      AdminAuthenticator
	httpClient *http.Client
}

// NewClient creates a new OpenBao client. The admin authenticator
// supplies the token for admin-scoped calls; when those return 403
// the client invokes admin.Reauth and retries the request once.
func NewClient(addr string, admin AdminAuthenticator) *Client {
	return &Client{
		addr:       strings.TrimRight(addr, "/"),
		admin:      admin,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Addr returns the OpenBao server address.
func (c *Client) Addr() string { return c.addr }

// Health checks if OpenBao is reachable and unsealed.
// GET {addr}/v1/sys/health
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.addr+"/v1/sys/health", nil)
	if err != nil {
		return fmt.Errorf("openbao health: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("openbao health: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// 200 = initialized, unsealed, active
	// 429 = unsealed, standby
	// 472 = data recovery mode
	// 473 = performance standby
	// 501 = not initialized
	// 503 = sealed
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTooManyRequests {
		return nil
	}
	return fmt.Errorf("openbao health: unexpected status %d", resp.StatusCode)
}

// doAdmin executes an admin-scoped request against the vault REST
// API. It sets the X-Vault-Token header from the admin authenticator
// and, on a 403 response, triggers a re-login via admin.Reauth and
// retries the request once with the refreshed token. Any 403 on the
// retry (or a Reauth failure) is surfaced as an error — the fast
// failure lets callers distinguish "credentials are bad" from
// "transient vault glitch".
//
// The body argument is optional; when non-nil it is sent with
// Content-Type application/json. The body bytes are reused across
// the retry.
func (c *Client) doAdmin(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		tok := c.admin.Token()

		var reqBody io.Reader
		if body != nil {
			reqBody = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.addr+path, reqBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Vault-Token", tok)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusForbidden || attempt > 0 {
			return resp, nil
		}

		// 403 on the first attempt: drain, reauth, retry.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if rerr := c.admin.Reauth(ctx, tok); rerr != nil {
			return nil, fmt.Errorf("vault %s %s: 403 and reauth failed: %w", method, path, rerr)
		}
	}
}

// jwtLoginResponse is the relevant subset of OpenBao's auth/jwt/login response.
type jwtLoginResponse struct {
	Auth struct {
		ClientToken   string `json:"client_token"`
		LeaseDuration int    `json:"lease_duration"`
	} `json:"auth"`
	Errors []string `json:"errors"`
}

// JWTLogin exchanges an IdP access token for a scoped OpenBao token.
// POST {addr}/v1/auth/{mountPath}/login
func (c *Client) JWTLogin(ctx context.Context, mountPath, accessToken string) (token string, ttl time.Duration, err error) {
	body := fmt.Sprintf(`{"role":"blockyard-user","jwt":%q}`, accessToken)
	url := fmt.Sprintf("%s/v1/auth/%s/login", c.addr, mountPath)

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("openbao jwt login: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("openbao jwt login: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", 0, fmt.Errorf("openbao jwt login: status %d", resp.StatusCode)
	}

	var result jwtLoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", 0, fmt.Errorf("openbao jwt login: decode response: %w", err)
	}

	if result.Auth.ClientToken == "" {
		return "", 0, fmt.Errorf("openbao jwt login: empty client_token")
	}

	return result.Auth.ClientToken, time.Duration(result.Auth.LeaseDuration) * time.Second, nil
}

// KVWrite writes a secret to the KV v2 secrets engine using the admin
// token. PUT {addr}/v1/secret/data/{path}
func (c *Client) KVWrite(ctx context.Context, path string, data map[string]any) error {
	payload, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return fmt.Errorf("openbao kv write: marshal: %w", err)
	}

	resp, err := c.doAdmin(ctx, "PUT", "/v1/secret/data/"+path, payload)
	if err != nil {
		return fmt.Errorf("openbao kv write: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("openbao kv write: status %d", resp.StatusCode)
	}
	return nil
}

// SecretExists checks whether a secret exists at the given KV v2 data
// path without reading its value. It queries the metadata endpoint
// (secret/metadata/...) which requires only metadata-read permission.
// GET {addr}/v1/secret/metadata/{path}
func (c *Client) SecretExists(ctx context.Context, path string) (bool, error) {
	// Convert data path to metadata path for existence check.
	metaPath := strings.Replace(path, "secret/data/", "secret/metadata/", 1)

	resp, err := c.doAdmin(ctx, "GET", "/v1/"+metaPath, nil)
	if err != nil {
		return false, fmt.Errorf("openbao secret exists: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("openbao secret exists: status %d", resp.StatusCode)
	}
	return true, nil
}

// kvReadResponse is the relevant subset of OpenBao's KV v2 read response.
type kvReadResponse struct {
	Data struct {
		Data map[string]any `json:"data"`
	} `json:"data"`
}

// KVRead reads a secret from the KV v2 secrets engine using the
// supplied (caller-provided) token. Used for per-user reads where
// the token comes from JWTLogin; for admin-scoped reads use
// KVReadAdmin, which handles token refresh on 403.
// GET {addr}/v1/secret/data/{path}
func (c *Client) KVRead(ctx context.Context, path string, token string) (map[string]any, error) {
	url := fmt.Sprintf("%s/v1/secret/data/%s", c.addr, path)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("openbao kv read: %w", err)
	}
	req.Header.Set("X-Vault-Token", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openbao kv read: %w", err)
	}
	defer resp.Body.Close()

	return parseKVReadResponse(resp, path)
}

// KVReadAdmin reads a secret from the KV v2 secrets engine using the
// admin token, with transparent 403-retry via admin.Reauth.
// GET {addr}/v1/secret/data/{path}
func (c *Client) KVReadAdmin(ctx context.Context, path string) (map[string]any, error) {
	resp, err := c.doAdmin(ctx, "GET", "/v1/secret/data/"+path, nil)
	if err != nil {
		return nil, fmt.Errorf("openbao kv read: %w", err)
	}
	defer resp.Body.Close()

	return parseKVReadResponse(resp, path)
}

func parseKVReadResponse(resp *http.Response, path string) (map[string]any, error) {
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("openbao kv read: %s: %w", path, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openbao kv read: status %d", resp.StatusCode)
	}

	var result kvReadResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("openbao kv read: decode: %w", err)
	}
	return result.Data.Data, nil
}
