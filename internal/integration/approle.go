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

	"golang.org/x/sync/singleflight"
)

// reloginBuffer is how far before the current token's expiry the
// proactive re-login timer fires. Picked to cover normal vault
// round-trip + a healthy safety margin; a 30s buffer on a 1h token
// only re-logs in once per TTL instead of once-per-TTL-minus-epsilon.
const reloginBuffer = 30 * time.Second

// appRoleLoginResponse is the relevant subset of the AppRole login response.
type appRoleLoginResponse struct {
	Auth struct {
		ClientToken   string `json:"client_token"`
		LeaseDuration int    `json:"lease_duration"`
	} `json:"auth"`
	Errors []string `json:"errors"`
}

// appRoleLogin performs an unsingleflighted AppRole login. Callers
// should normally go through AppRoleAuth.Login so concurrent re-logins
// coalesce into one call.
func appRoleLogin(ctx context.Context, httpClient *http.Client, addr, roleID, secretID string) (token string, ttl time.Duration, err error) {
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

// AppRoleAuth maintains a vault admin token by logging in against the
// AppRole endpoint. It re-reads the secret_id from disk on every login
// (when SecretIDFile is set), so operators can rotate secret_id without
// restarting blockyard. A proactive timer triggers a re-login shortly
// before the current token expires; 403 responses on admin calls
// trigger an on-demand re-login through the same singleflight-coalesced
// path.
type AppRoleAuth struct {
	addr         string
	roleID       string
	secretIDFile string // empty → read BLOCKYARD_VAULT_SECRET_ID from the process environment
	httpClient   *http.Client

	token    atomic.Value // string
	expireAt atomic.Value // time.Time — zero until first successful login
	healthy  atomic.Bool

	group singleflight.Group

	// timerMu guards the scheduling state updated from both Login and Run.
	timerMu sync.Mutex
	nextAt  time.Time
}

// NewAppRoleAuth constructs an AppRoleAuth. secretIDFile may be empty,
// in which case the secret_id is read from BLOCKYARD_VAULT_SECRET_ID at
// each login attempt. The underlying http.Client uses system CA trust
// and a 10s timeout; call WithHTTPClient to override (e.g. for a
// private CA).
func NewAppRoleAuth(addr, roleID, secretIDFile string) *AppRoleAuth {
	a := &AppRoleAuth{
		addr:         strings.TrimRight(addr, "/"),
		roleID:       roleID,
		secretIDFile: secretIDFile,
		httpClient:   DefaultHTTPClient(),
	}
	a.token.Store("")
	a.expireAt.Store(time.Time{})
	return a
}

// WithHTTPClient replaces the underlying http.Client. Returns the
// receiver to allow chaining from NewAppRoleAuth.
func (a *AppRoleAuth) WithHTTPClient(h *http.Client) *AppRoleAuth {
	a.httpClient = h
	return a
}

// Token returns the current vault admin token. Empty until a successful
// login has completed. Safe for concurrent use.
func (a *AppRoleAuth) Token() string {
	s, _ := a.token.Load().(string)
	return s
}

// Healthy returns true when the last login attempt succeeded and the
// stored token has not yet expired. Drives the "vault" readyz/preflight
// component.
func (a *AppRoleAuth) Healthy() bool {
	if !a.healthy.Load() {
		return false
	}
	exp, _ := a.expireAt.Load().(time.Time)
	return !exp.IsZero() && time.Now().Before(exp)
}

// Login performs an AppRole login, stores the resulting token and
// schedules the next proactive re-login. Concurrent callers coalesce:
// only one login hits the wire, the others wait for and share its
// result. Safe to call at any time (startup, 403-retry, timer fire).
func (a *AppRoleAuth) Login(ctx context.Context) error {
	_, err, _ := a.group.Do("login", func() (any, error) {
		return nil, a.doLogin(ctx)
	})
	return err
}

func (a *AppRoleAuth) doLogin(ctx context.Context) error {
	secretID, err := a.resolveSecretID()
	if err != nil {
		a.healthy.Store(false)
		return err
	}

	token, ttl, err := appRoleLogin(ctx, a.httpClient, a.addr, a.roleID, secretID)
	if err != nil {
		a.healthy.Store(false)
		return err
	}

	a.token.Store(token)
	a.expireAt.Store(time.Now().Add(ttl))
	a.healthy.Store(true)

	a.timerMu.Lock()
	a.nextAt = time.Now().Add(ttl - reloginBuffer)
	a.timerMu.Unlock()

	slog.Info("vault AppRole login succeeded", "ttl", ttl)
	return nil
}

// resolveSecretID returns the secret_id to use for the next login. When
// SecretIDFile is set, the file is re-read every call so a rotation on
// disk is picked up immediately. Otherwise BLOCKYARD_VAULT_SECRET_ID is
// read from the process environment.
func (a *AppRoleAuth) resolveSecretID() (string, error) {
	if a.secretIDFile != "" {
		data, err := os.ReadFile(a.secretIDFile) //nolint:gosec // G304: operator-configured path
		if err != nil {
			return "", fmt.Errorf("read secret_id file %q: %w", a.secretIDFile, err)
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			return "", fmt.Errorf("secret_id file %q is empty", a.secretIDFile)
		}
		return s, nil
	}
	s := os.Getenv("BLOCKYARD_VAULT_SECRET_ID")
	if s == "" {
		return "", errors.New("set BLOCKYARD_VAULT_SECRET_ID or vault.secret_id_file")
	}
	return s, nil
}

// Run blocks until ctx is cancelled, re-logging in shortly before each
// token expires. Failed re-logins mark the component unhealthy but do
// not exit the loop — a later retry (or a 403-driven retry via Login)
// can recover. Backoff is simple and short because the 403-retry path
// on admin calls provides the actual recovery signal.
func (a *AppRoleAuth) Run(ctx context.Context) {
	for {
		a.timerMu.Lock()
		wait := time.Until(a.nextAt)
		a.timerMu.Unlock()

		if wait < time.Second {
			wait = time.Second
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if err := a.Login(ctx); err != nil {
			slog.Warn("vault AppRole re-login failed", "error", err)
			// On failure, retry sooner but not in a tight loop. The
			// proactive timer isn't the load-bearing recovery path; if
			// an admin call 403s in the meantime, that call re-logs in
			// through the same singleflight and kicks nextAt forward.
			a.timerMu.Lock()
			a.nextAt = time.Now().Add(10 * time.Second)
			a.timerMu.Unlock()
		}
	}
}
