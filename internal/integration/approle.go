package integration

import (
	"context"
	"crypto/sha256"
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
	addr            string
	roleID          string
	secretIDFile    string // empty → read BLOCKYARD_VAULT_SECRET_ID from the process environment
	secretIDWrapped bool   // when true, file contents are a response-wrap token to unwrap at login time
	httpClient      *http.Client

	token    atomic.Value // string
	expireAt atomic.Value // time.Time — zero until first successful login
	healthy  atomic.Bool

	group singleflight.Group

	// timerMu guards the scheduling state updated from both Login and Run.
	timerMu sync.Mutex
	nextAt  time.Time

	// kick is a buffered (cap 1) signal from doLogin to Run that nextAt
	// was updated outside the proactive-timer path (startup Login, a
	// 403-driven re-login). Run re-arms its timer against the fresh
	// nextAt so the already-armed timer doesn't fire a redundant
	// second login at its originally-scheduled time.
	kick chan struct{}

	// Wrap-unwrap cache. Wrap tokens are single-use, so re-unwrapping
	// the same bytes fails — we cache the plaintext keyed by the SHA-256
	// of the file contents and only re-unwrap when the file changes.
	// Accessed only from resolveSecretID, which runs under the "login"
	// singleflight, so no lock is needed.
	unwrapHash      [sha256.Size]byte
	unwrapPlaintext string
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
		kick:         make(chan struct{}, 1),
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

// WithSecretIDWrapped enables response-wrap mode: the secret_id file
// is treated as a vault wrap token, and the real secret_id is fetched
// via sys/wrapping/unwrap on every login for which the file has
// changed. Requires SecretIDFile to be set. Returns the receiver to
// allow chaining from NewAppRoleAuth.
func (a *AppRoleAuth) WithSecretIDWrapped(b bool) *AppRoleAuth {
	a.secretIDWrapped = b
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
	secretID, err := a.resolveSecretID(ctx)
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

	// Non-blocking: signal Run to re-arm its timer against the new
	// nextAt. A full buffer means a previous kick is still pending;
	// Run will read the latest nextAt when it drains either way.
	select {
	case a.kick <- struct{}{}:
	default:
	}

	slog.Info("vault AppRole login succeeded", "ttl", ttl)
	return nil
}

// resolveSecretID returns the secret_id to use for the next login. When
// SecretIDFile is set, the file is re-read every call so a rotation on
// disk is picked up immediately. In wrap mode the file contents are a
// single-use response-wrap token; the plaintext secret_id is cached
// against the file's hash so proactive re-logins don't burn extra
// unwraps against the same rotation. Without SecretIDFile, the
// BLOCKYARD_VAULT_SECRET_ID env var is used.
func (a *AppRoleAuth) resolveSecretID(ctx context.Context) (string, error) {
	if a.secretIDFile != "" {
		data, err := os.ReadFile(a.secretIDFile) //nolint:gosec // G304: operator-configured path
		if err != nil {
			return "", fmt.Errorf("read secret_id file %q: %w", a.secretIDFile, err)
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			return "", fmt.Errorf("secret_id file %q is empty", a.secretIDFile)
		}
		if !a.secretIDWrapped {
			return s, nil
		}
		hash := sha256.Sum256([]byte(s))
		if hash == a.unwrapHash && a.unwrapPlaintext != "" {
			return a.unwrapPlaintext, nil
		}
		plaintext, err := unwrapSecretID(ctx, a.httpClient, a.addr, s)
		if err != nil {
			return "", fmt.Errorf("unwrap secret_id file %q: %w", a.secretIDFile, err)
		}
		a.unwrapHash = hash
		a.unwrapPlaintext = plaintext
		return plaintext, nil
	}
	s := os.Getenv("BLOCKYARD_VAULT_SECRET_ID")
	if s == "" {
		return "", errors.New("set BLOCKYARD_VAULT_SECRET_ID or vault.secret_id_file")
	}
	return s, nil
}

// wrappingUnwrapResponse is the relevant subset of the vault
// sys/wrapping/unwrap response when the wrapped payload was a
// {"secret_id": ..., "secret_id_accessor": ...} envelope (the shape
// `vault write -f -wrap-ttl=… auth/approle/role/<name>/secret-id`
// produces).
type wrappingUnwrapResponse struct {
	Data struct {
		SecretID string `json:"secret_id"`
	} `json:"data"`
	Errors []string `json:"errors"`
}

// unwrapSecretID POSTs a wrap token to sys/wrapping/unwrap and returns
// the enclosed secret_id. Wrap tokens are single-use: a repeat unwrap
// of the same token fails, which is the intended tamper signal when a
// hostile process consumed the token before blockyard did.
func unwrapSecretID(ctx context.Context, httpClient *http.Client, addr, wrapToken string) (string, error) {
	url := strings.TrimRight(addr, "/") + "/v1/sys/wrapping/unwrap"
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return "", fmt.Errorf("unwrap: %w", err)
	}
	req.Header.Set("X-Vault-Token", wrapToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("unwrap: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("unwrap: status %d", resp.StatusCode)
	}

	var result wrappingUnwrapResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("unwrap: decode: %w", err)
	}
	if result.Data.SecretID == "" {
		return "", fmt.Errorf("unwrap: empty secret_id in response")
	}
	return result.Data.SecretID, nil
}

// Run blocks until ctx is cancelled, re-logging in shortly before each
// token expires. Failed re-logins mark the component unhealthy but do
// not exit the loop — a later retry (or a 403-driven retry via Login)
// can recover. Retries use exponential backoff (1s → 60s cap) because
// the 403-retry path on admin calls provides the actual recovery signal,
// so the proactive loop only needs to keep trying without flooding vault.
//
// A Login that completes outside this loop (startup bootstrap, 403
// retry) signals via a.kick so Run re-arms against the fresh nextAt,
// avoiding a redundant second login at the previously-scheduled time.
func (a *AppRoleAuth) Run(ctx context.Context) {
	backoff := 1 * time.Second
	const maxBackoff = 60 * time.Second

	timer := time.NewTimer(a.waitUntilNext())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := a.Login(ctx); err != nil {
				slog.Warn("vault AppRole re-login failed", "error", err, "retry_in", backoff)
				// On failure, retry with exponential backoff. If an
				// admin call 403s in the meantime, that call re-logs in
				// through the same singleflight and kicks nextAt forward.
				a.timerMu.Lock()
				a.nextAt = time.Now().Add(backoff)
				a.timerMu.Unlock()
				backoff = min(backoff*2, maxBackoff)
			} else {
				backoff = 1 * time.Second
			}
			// Drain the self-kick our own successful Login just emitted
			// so the next loop iteration doesn't re-arm for no reason.
			select {
			case <-a.kick:
			default:
			}
			timer.Reset(a.waitUntilNext())
		case <-a.kick:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(a.waitUntilNext())
		}
	}
}

// waitUntilNext returns how long until the currently-scheduled re-login
// should fire, clamped to a 1s minimum so a missed deadline still gives
// the vault round-trip room to land.
func (a *AppRoleAuth) waitUntilNext() time.Duration {
	a.timerMu.Lock()
	defer a.timerMu.Unlock()
	w := time.Until(a.nextAt)
	if w < time.Second {
		w = time.Second
	}
	return w
}
