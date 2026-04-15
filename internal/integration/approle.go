package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// AppRoleCreds describes how role_id and secret_id are sourced at
// AppRole-login time. Each field accepts either a literal value
// (captured once, typically from an env var or the TOML config) or a
// file path, which is re-read on every Resolve call so rotations on
// disk take effect without a restart. When both the env and file
// source are set for the same field, the file wins.
type AppRoleCreds struct {
	RoleIDEnv    string
	RoleIDFile   string
	SecretIDEnv  string
	SecretIDFile string
}

// Resolve returns the current role_id and secret_id. File-backed
// fields are re-read on each call; env-backed fields return the
// captured value.
func (c AppRoleCreds) Resolve() (roleID, secretID string, err error) {
	roleID, err = readCred(c.RoleIDFile, c.RoleIDEnv, "BLOCKYARD_OPENBAO_ROLE_ID")
	if err != nil {
		return "", "", err
	}
	secretID, err = readCred(c.SecretIDFile, c.SecretIDEnv, "BLOCKYARD_OPENBAO_SECRET_ID")
	if err != nil {
		return "", "", err
	}
	return roleID, secretID, nil
}

func readCred(file, envVal, envName string) (string, error) {
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read %s from %q: %w", envName, file, err)
		}
		val := strings.TrimSpace(string(data))
		if val == "" {
			return "", fmt.Errorf("%s file %q is empty", envName, file)
		}
		return val, nil
	}
	if envVal == "" {
		return "", fmt.Errorf("neither %s nor %s_FILE is set", envName, envName)
	}
	return envVal, nil
}

// AppRoleCredsFromEnv builds AppRoleCreds using the standard
// BLOCKYARD_OPENBAO_* env vars for file paths and the supplied
// roleID (typically cfg.Openbao.RoleID) as the env-sourced role_id.
// The secret_id env value is read from BLOCKYARD_OPENBAO_SECRET_ID.
func AppRoleCredsFromEnv(roleID string) AppRoleCreds {
	return AppRoleCreds{
		RoleIDEnv:    roleID,
		RoleIDFile:   os.Getenv("BLOCKYARD_OPENBAO_ROLE_ID_FILE"),
		SecretIDEnv:  os.Getenv("BLOCKYARD_OPENBAO_SECRET_ID"),
		SecretIDFile: os.Getenv("BLOCKYARD_OPENBAO_SECRET_ID_FILE"),
	}
}

// AppRoleAuth owns a cached Vault token obtained via AppRole login.
// Callers get the current token via Token for their requests and
// trigger Reauth when the server returns 403; Reauth re-resolves
// credentials (re-reading any _FILE sources), performs a fresh
// login, and updates the cached token. Concurrent Reauth calls are
// singleflight-coalesced so only one login is in flight at a time.
type AppRoleAuth struct {
	addr       string
	httpClient *http.Client
	creds      AppRoleCreds

	token atomic.Value // string

	mu       sync.Mutex
	inflight *inflightLogin
}

type inflightLogin struct {
	done chan struct{}
	err  error
}

// NewAppRoleAuth constructs an AppRoleAuth against the given vault
// address. The initial token is empty; call Login to populate it
// before issuing authenticated requests.
func NewAppRoleAuth(addr string, creds AppRoleCreds) *AppRoleAuth {
	a := &AppRoleAuth{
		addr:       strings.TrimRight(addr, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		creds:      creds,
	}
	a.token.Store("")
	return a
}

// Token returns the currently cached token. Empty until Login or
// Reauth has succeeded at least once.
func (a *AppRoleAuth) Token() string {
	return a.token.Load().(string)
}

// Login performs the initial AppRole login. Intended to be called
// once at startup before the client issues authenticated requests.
func (a *AppRoleAuth) Login(ctx context.Context) error {
	return a.refresh(ctx, "")
}

// Reauth refreshes the cached token after a caller observed a 403
// using observedToken. If another goroutine has already rotated the
// token (the cached token no longer matches observedToken), Reauth
// returns nil without contacting the server — the caller should
// retry with the current Token. Otherwise a login is initiated (or
// joined, when another goroutine is already logging in) and the
// result shared.
func (a *AppRoleAuth) Reauth(ctx context.Context, observedToken string) error {
	return a.refresh(ctx, observedToken)
}

func (a *AppRoleAuth) refresh(ctx context.Context, observedToken string) error {
	a.mu.Lock()

	// Another caller already rotated the token; caller just needs to
	// retry with Token().
	if observedToken != "" && a.Token() != observedToken {
		a.mu.Unlock()
		return nil
	}

	if a.inflight != nil {
		inflight := a.inflight
		a.mu.Unlock()
		select {
		case <-inflight.done:
			return inflight.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	inflight := &inflightLogin{done: make(chan struct{})}
	a.inflight = inflight
	a.mu.Unlock()

	err := a.login(ctx)

	a.mu.Lock()
	inflight.err = err
	close(inflight.done)
	a.inflight = nil
	a.mu.Unlock()

	return err
}

func (a *AppRoleAuth) login(ctx context.Context) error {
	roleID, secretID, err := a.creds.Resolve()
	if err != nil {
		return fmt.Errorf("approle auth: resolve credentials: %w", err)
	}
	token, _, err := AppRoleLogin(ctx, a.httpClient, a.addr, roleID, secretID)
	if err != nil {
		return fmt.Errorf("approle auth: %w", err)
	}
	a.token.Store(token)
	slog.Info("vault AppRole login successful")
	return nil
}

// AdminAuthenticator provides the token for admin-scoped OpenBao
// calls, with an optional re-authentication hook used by the client
// to recover from 403 responses.
type AdminAuthenticator interface {
	// Token returns the current admin token.
	Token() string
	// Reauth is called after a caller observed a 403 using
	// observedToken. Implementations rotate the cached token if
	// possible and return nil, or return a non-nil error if no
	// refresh is supported.
	Reauth(ctx context.Context, observedToken string) error
}

// ErrStaticAdminCannotReauth is returned by the static admin_token
// path to signal that a 403 is unrecoverable without an operator
// rotation or a migration to AppRole auth (role_id).
var ErrStaticAdminCannotReauth = errors.New(
	"static admin_token cannot be re-issued; set role_id to enable AppRole auth")

type staticAdmin struct {
	get func() string
}

// StaticAdmin wraps a fixed admin token callback. Reauth always
// returns ErrStaticAdminCannotReauth.
func StaticAdmin(get func() string) AdminAuthenticator {
	return staticAdmin{get: get}
}

func (s staticAdmin) Token() string { return s.get() }
func (s staticAdmin) Reauth(ctx context.Context, observedToken string) error {
	return ErrStaticAdminCannotReauth
}
