package integration

import (
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
	addr           string
	adminTokenFunc func() string
	httpClient     *http.Client
}

// NewClient creates a new OpenBao client. The adminToken is retrieved
// via a callback to avoid holding the plaintext value in a long-lived
// struct field.
func NewClient(addr string, adminTokenFunc func() string) *Client {
	return &Client{
		addr:           strings.TrimRight(addr, "/"),
		adminTokenFunc: adminTokenFunc,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Addr returns the OpenBao server address.
func (c *Client) Addr() string { return c.addr }

// AdminToken returns the current admin token. Satisfies
// config.SecretResolver.
func (c *Client) AdminToken() string { return c.adminTokenFunc() }

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

// KVWrite writes a secret to the KV v2 secrets engine using the admin token.
// PUT {addr}/v1/secret/data/{path}
func (c *Client) KVWrite(ctx context.Context, path string, data map[string]any) error {
	payload, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return fmt.Errorf("openbao kv write: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/v1/secret/data/%s", c.addr, path)
	req, err := http.NewRequestWithContext(ctx, "PUT", url, strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("openbao kv write: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Token", c.adminTokenFunc())

	resp, err := c.httpClient.Do(req)
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

	url := fmt.Sprintf("%s/v1/%s", c.addr, metaPath)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, fmt.Errorf("openbao secret exists: %w", err)
	}
	req.Header.Set("X-Vault-Token", c.adminTokenFunc())

	resp, err := c.httpClient.Do(req)
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

// DatabaseStaticRoleCreate registers (or updates) a static DB role on
// vault's `database` secrets engine. Called from blockyard's
// first-login flow for board storage (#284): after the per-user PG
// role user_<sub> exists, this tells vault to adopt it and start
// rotating its password on the given period. Vault immediately
// rotates the temporary password set at creation time; subsequent
// reads of `{mount}/static-creds/{name}` return the current one.
//
// Idempotent: vault returns 200/204 on update of an existing role.
//
// Uses the admin AppRole token configured via [openbao].
// POST {addr}/v1/{mount}/static-roles/{name}
func (c *Client) DatabaseStaticRoleCreate(
	ctx context.Context,
	mount, name, username, dbName, rotationPeriod string,
) error {
	payload, err := json.Marshal(map[string]any{
		"username":        username,
		"db_name":         dbName,
		"rotation_period": rotationPeriod,
	})
	if err != nil {
		return fmt.Errorf("openbao db static-role create: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/v1/%s/static-roles/%s", c.addr, mount, name)
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("openbao db static-role create: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Token", c.adminTokenFunc())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("openbao db static-role create: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("openbao db static-role create %s: status %d", name, resp.StatusCode)
	}
	return nil
}

// dbStaticCredsResponse is the relevant subset of OpenBao's database
// secrets-engine static-role credential response.
type dbStaticCredsResponse struct {
	Data struct {
		Username string `json:"username"`
		Password string `json:"password"`
		TTL      int    `json:"ttl"`
	} `json:"data"`
}

// DatabaseStaticCredsRead fetches the current username/password for a
// vault static DB role. Vault manages the PG user out-of-band (the
// operator registers the role; vault rotates its password on a
// schedule) — this call just reads the current credentials, which are
// stable between rotations.
//
// Unlike dynamic creds (`{mount}/creds/{role}`), static-creds leases
// are not tied to the caller's token, so blockyard can read with its
// own AppRole token without inheriting a lease that expires with the
// token. Returned TTL reflects vault's time-to-next-rotation, not a
// lease bound to this caller.
//
// Used for admin creds (#238, via cfg.Database.VaultRole) and by
// workers for per-user creds (#284, `{mount}/static-creds/user_<id>`).
// GET {addr}/v1/{mount}/static-creds/{name}
func (c *Client) DatabaseStaticCredsRead(
	ctx context.Context, mount, name string,
) (username, password string, ttl time.Duration, err error) {
	url := fmt.Sprintf("%s/v1/%s/static-creds/%s", c.addr, mount, name)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", "", 0, fmt.Errorf("openbao db static-creds: %w", err)
	}
	req.Header.Set("X-Vault-Token", c.adminTokenFunc())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("openbao db static-creds: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", "", 0, fmt.Errorf("openbao db static-creds %s: %w", name, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", "", 0, fmt.Errorf("openbao db static-creds %s: status %d", name, resp.StatusCode)
	}

	var result dbStaticCredsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, fmt.Errorf("openbao db static-creds: decode: %w", err)
	}
	if result.Data.Username == "" || result.Data.Password == "" {
		return "", "", 0, fmt.Errorf("openbao db static-creds %s: empty username or password", name)
	}
	return result.Data.Username, result.Data.Password, time.Duration(result.Data.TTL) * time.Second, nil
}

// AuthMountAccessor returns the opaque accessor of the auth method
// mounted at `path` (e.g. "jwt"). Accessors are vault-internal
// identifiers distinct from mount paths; the alias lookup below
// requires them and operators rarely know them out-of-band.
//
// Called once at startup from board-storage provisioning (#285) to
// cache the OIDC mount accessor — identity/lookup/entity expects it
// whereas operators configure the mount by path.
// GET {addr}/v1/sys/auth
func (c *Client) AuthMountAccessor(ctx context.Context, path string) (string, error) {
	url := fmt.Sprintf("%s/v1/sys/auth", c.addr)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("openbao sys/auth: %w", err)
	}
	req.Header.Set("X-Vault-Token", c.adminTokenFunc())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("openbao sys/auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("openbao sys/auth: status %d", resp.StatusCode)
	}

	// Vault's sys/auth response mixes two shapes: mount-path entries
	// keyed like "jwt/" alongside top-level metadata fields
	// (request_id, lease_id, renewable, wrap_info, warnings, auth,
	// data). Decoding straight into map[string]struct{Accessor} fails
	// because the metadata fields aren't objects. Hold the whole
	// envelope as RawMessage and only decode the one entry we need.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return "", fmt.Errorf("openbao sys/auth: decode: %w", err)
	}

	key := strings.TrimSuffix(path, "/") + "/"
	entryRaw, ok := raw[key]
	if !ok {
		// Some vault versions also nest the auth methods under
		// "data". Fall back to that shape before declaring missing.
		if data, dataOk := raw["data"]; dataOk {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(data, &nested); err == nil {
				entryRaw, ok = nested[key]
			}
		}
		if !ok {
			return "", fmt.Errorf("openbao sys/auth: no auth method at %q", path)
		}
	}
	var entry struct {
		Accessor string `json:"accessor"`
	}
	if err := json.Unmarshal(entryRaw, &entry); err != nil {
		return "", fmt.Errorf("openbao sys/auth: decode %s: %w", path, err)
	}
	if entry.Accessor == "" {
		return "", fmt.Errorf("openbao sys/auth: accessor missing at %q", path)
	}
	return entry.Accessor, nil
}

// IdentityLookupEntityByAlias resolves the vault entity ID for the
// given (aliasName, aliasMountAccessor) pair. Vault assigns entity
// UUIDs the first time it sees an alias (e.g. on OIDC first login),
// so this call is well-defined only after the user has logged in
// through the given auth mount at least once.
//
// Used by board-storage provisioning (#285) to derive a stable PG role
// name `user_<entity-id>` — the entity ID is the same identifier the
// templated per-user vault policy resolves at the ACL layer, so the
// two sides agree without blockyard writing anything on vault's
// identity side.
// POST {addr}/v1/identity/lookup/entity
func (c *Client) IdentityLookupEntityByAlias(
	ctx context.Context, aliasName, aliasMountAccessor string,
) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"alias_name":           aliasName,
		"alias_mount_accessor": aliasMountAccessor,
	})
	if err != nil {
		return "", fmt.Errorf("openbao identity lookup: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/v1/identity/lookup/entity", c.addr)
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(payload)))
	if err != nil {
		return "", fmt.Errorf("openbao identity lookup: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Token", c.adminTokenFunc())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("openbao identity lookup: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("openbao identity lookup: alias %q: %w", aliasName, ErrNotFound)
	}
	// Vault returns 204 No Content when the alias is unknown (no
	// entity exists yet). Treat as not-found for caller clarity.
	if resp.StatusCode == http.StatusNoContent {
		return "", fmt.Errorf("openbao identity lookup: alias %q: %w", aliasName, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openbao identity lookup: status %d", resp.StatusCode)
	}

	var result struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("openbao identity lookup: decode: %w", err)
	}
	if result.Data.ID == "" {
		return "", fmt.Errorf("openbao identity lookup: alias %q: empty entity id", aliasName)
	}
	return result.Data.ID, nil
}

// kvReadResponse is the relevant subset of OpenBao's KV v2 read response.
type kvReadResponse struct {
	Data struct {
		Data map[string]any `json:"data"`
	} `json:"data"`
}

// KVRead reads a secret from the KV v2 secrets engine.
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
