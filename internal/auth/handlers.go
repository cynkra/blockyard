package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cynkra/blockyard/internal/config"
)

// nowUnix returns the current time as a unix timestamp. Declared as a
// package variable so tests can override it.
var nowUnix = func() int64 {
	return time.Now().Unix()
}

// NowUnix is the exported accessor for nowUnix, used by tests.
func NowUnix() int64 { return nowUnix() }

// Deps carries the dependencies that auth handlers and middleware need.
// Constructed in the router layer from the server struct, avoiding a
// circular import between auth and server.
type Deps struct {
	Config       *config.Config
	OIDCClient   *OIDCClient
	SigningKey    *SigningKey
	UserSessions *UserSessionStore
}

// secureFlag returns "; Secure" if external_url is HTTPS, empty
// string otherwise.
func secureFlag(cfg *config.Config) string {
	if strings.HasPrefix(cfg.Server.ExternalURL, "https://") {
		return "; Secure"
	}
	return ""
}

// randomHex generates a cryptographically random hex string of n bytes.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type oidcStatePayload struct {
	CSRFToken string `json:"csrf_token"`
	Nonce     string `json:"nonce"`
	ReturnURL string `json:"return_url"`
}

func buildStateCookie(payload *oidcStatePayload, key *SigningKey, cfg *config.Config) (string, error) {
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(jsonBytes)

	mac := hmac.New(sha256.New, key.key)
	mac.Write(jsonBytes)
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	value := encoded + "." + sig
	secure := secureFlag(cfg)
	return fmt.Sprintf(
		"blockyard_oidc_state=%s; Path=/; HttpOnly; SameSite=Lax%s; Max-Age=300",
		value, secure,
	), nil
}

func extractStateCookie(r *http.Request, key *SigningKey) (*oidcStatePayload, error) {
	cookie, err := r.Cookie("blockyard_oidc_state")
	if err != nil {
		return nil, fmt.Errorf("missing oidc state cookie: %w", err)
	}

	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed state cookie")
	}

	jsonBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode state cookie: %w", err)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode state signature: %w", err)
	}

	mac := hmac.New(sha256.New, key.key)
	mac.Write(jsonBytes)
	if !hmac.Equal(sigBytes, mac.Sum(nil)) {
		return nil, errors.New("invalid state cookie signature")
	}

	var payload oidcStatePayload
	if err := json.Unmarshal(jsonBytes, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal state cookie: %w", err)
	}
	return &payload, nil
}

// LoginHandler initiates the OIDC authorization code flow.
// Query params: ?return_url=/app/my-app/ (optional, default: /)
func LoginHandler(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.OIDCClient == nil {
			http.NotFound(w, r)
			return
		}

		state := randomHex(16)
		nonce := randomHex(16)

		authURL := deps.OIDCClient.AuthCodeURL(state, nonce)

		// Validate return_url to prevent open redirect attacks.
		returnURL := r.URL.Query().Get("return_url")
		if !strings.HasPrefix(returnURL, "/") || strings.HasPrefix(returnURL, "//") {
			returnURL = "/"
		}

		statePayload := oidcStatePayload{
			CSRFToken: state,
			Nonce:     nonce,
			ReturnURL: returnURL,
		}
		stateCookie, err := buildStateCookie(&statePayload, deps.SigningKey, deps.Config)
		if err != nil {
			slog.Error("failed to build state cookie", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Add("Set-Cookie", stateCookie)
		http.Redirect(w, r, authURL, http.StatusFound)
	}
}

// CallbackHandler handles the IdP callback after user authentication.
func CallbackHandler(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.OIDCClient == nil {
			http.NotFound(w, r)
			return
		}

		// 1. Extract and validate OIDC state cookie.
		statePayload, err := extractStateCookie(r, deps.SigningKey)
		if err != nil {
			slog.Error("invalid state cookie", "error", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// 2. Verify CSRF token matches.
		if r.URL.Query().Get("state") != statePayload.CSRFToken {
			http.Error(w, "CSRF token mismatch", http.StatusBadRequest)
			return
		}

		// 3. Exchange authorization code for tokens.
		code := r.URL.Query().Get("code")
		oauth2Token, _, allClaims, err := deps.OIDCClient.Exchange(r.Context(), code)
		if err != nil {
			slog.Error("token exchange failed", "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}

		// 4. Extract sub and groups.
		var subClaim string
		if raw, ok := allClaims["sub"]; ok {
			_ = json.Unmarshal(raw, &subClaim)
		}
		if subClaim == "" {
			slog.Error("ID token missing sub claim")
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		groups := deps.OIDCClient.ExtractGroups(allClaims)

		// 5. Store session server-side.
		expiresAt := nowUnix() + 300 // default 5 min
		if !oauth2Token.Expiry.IsZero() {
			expiresAt = oauth2Token.Expiry.Unix()
		}

		deps.UserSessions.Set(subClaim, &UserSession{
			Groups:       groups,
			AccessToken:  oauth2Token.AccessToken,
			RefreshToken: oauth2Token.RefreshToken,
			ExpiresAt:    expiresAt,
		})

		// 6. Build signed session cookie.
		cookiePayload := &CookiePayload{
			Sub:      subClaim,
			IssuedAt: nowUnix(),
		}
		cookieValue, err := cookiePayload.Encode(deps.SigningKey)
		if err != nil {
			slog.Error("failed to encode session cookie", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		cookieMaxAge := int64(24 * 60 * 60)
		if deps.Config.OIDC != nil {
			cookieMaxAge = int64(deps.Config.OIDC.CookieMaxAge.Duration.Seconds())
		}

		secure := secureFlag(deps.Config)
		sessionCookie := fmt.Sprintf(
			"blockyard_session=%s; Path=/; HttpOnly; SameSite=Lax%s; Max-Age=%d",
			cookieValue, secure, cookieMaxAge,
		)

		// 7. Clear the OIDC state cookie.
		clearState := fmt.Sprintf(
			"blockyard_oidc_state=; Path=/; HttpOnly%s; Max-Age=0", secure,
		)

		// 8. Redirect to return_url.
		w.Header().Add("Set-Cookie", sessionCookie)
		w.Header().Add("Set-Cookie", clearState)
		http.Redirect(w, r, statePayload.ReturnURL, http.StatusFound)
	}
}

// LogoutHandler clears the session cookie and removes the server-side
// session. Redirects to / (or to the IdP's end_session_endpoint if
// available).
func LogoutHandler(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SigningKey != nil && deps.UserSessions != nil {
			if cookieValue := extractSessionCookie(r); cookieValue != "" {
				if payload, err := DecodeCookie(cookieValue, deps.SigningKey); err == nil {
					deps.UserSessions.Delete(payload.Sub)
				}
			}
		}

		secure := secureFlag(deps.Config)
		clearCookie := fmt.Sprintf(
			"blockyard_session=; Path=/; HttpOnly%s; Max-Age=0", secure,
		)
		w.Header().Set("Set-Cookie", clearCookie)

		if deps.OIDCClient != nil {
			if endSession := deps.OIDCClient.EndSessionEndpoint(); endSession != "" {
				http.Redirect(w, r, endSession, http.StatusFound)
				return
			}
		}

		http.Redirect(w, r, "/", http.StatusFound)
	}
}
